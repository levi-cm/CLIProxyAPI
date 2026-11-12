package accountpolicyledger

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

type cursor struct {
	Name      string `json:"n"`
	Offset    int64  `json:"o"`
	End       string `json:"e"`
	EndOffset int64  `json:"z"`
	Filter    string `json:"f"`
}

func queryFingerprint(q Query) string {
	q.Cursor = ""
	q.Limit = 0
	b, _ := json.Marshal(q)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func matches(q Query, at time.Time, provider, model, credential string) bool {
	return (q.From.IsZero() || !at.Before(q.From)) && (q.To.IsZero() || at.Before(q.To)) && (q.Provider == "" || provider == q.Provider) && (q.Model == "" || model == q.Model) && (q.CredentialID == "" || credential == q.CredentialID)
}
func validQuery(q Query) error {
	if !q.From.IsZero() && !q.To.IsZero() && !q.To.After(q.From) {
		return errors.New("invalid time range")
	}
	if q.Limit < 0 || q.Limit > 500 {
		return errors.New("page limit must be 1 through 500")
	}
	return nil
}

// Requests anchors both ends of pagination to immutable append positions.
func (a *Archive) Requests(q Query) (Page, error) {
	return a.RequestsContext(context.Background(), q)
}

// RequestsContext scans immutable byte ranges outside the writer lock. Client
// cancellation is checked even when a filter matches no historical records.
func (a *Archive) RequestsContext(ctx context.Context, q Query) (Page, error) {
	p := Page{SchemaVersion: 1, Records: []Request{}}
	if err := validQuery(q); err != nil {
		return p, err
	}
	if a == nil {
		return p, errors.New("history unavailable")
	}
	root, segments, pos, err := a.requestPositions(q)
	if err != nil {
		return p, err
	}
	if root == nil {
		return p, ctx.Err()
	}
	defer root.Close()
	if err := ctx.Err(); err != nil {
		return p, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	for _, segment := range segments {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		if !requestSegmentInRange(root, segment, q) {
			continue
		}
		info, err := root.Lstat(segment.Name)
		if err != nil || !info.Mode().IsRegular() {
			return p, errors.New("unsafe history segment")
		}
		f, err := root.Open(segment.Name)
		if err != nil {
			return p, err
		}
		offset := int64(0)
		if segment.Name == pos.Name {
			offset = pos.Offset
		}
		if offset > segment.Size {
			_ = f.Close()
			return p, errors.New("invalid cursor offset")
		}
		reader := bufio.NewReader(io.NewSectionReader(f, offset, segment.Size-offset))
		for {
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return p, err
			}
			line, err := reader.ReadBytes('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = f.Close()
				return p, err
			}
			offset += int64(len(line))
			var item Request
			if json.Unmarshal(line, &item) != nil || item.SchemaVersion != SchemaVersion {
				continue
			}
			if !matches(q, item.RequestedAt, item.Provider, item.ExecutionModel, item.CredentialID) && !(q.Model != "" && matches(Query{From: q.From, To: q.To, Provider: q.Provider, CredentialID: q.CredentialID}, item.RequestedAt, item.Provider, item.ExecutionModel, item.CredentialID) && (item.RequestedModel == q.Model || item.ResponseModel == q.Model)) {
				continue
			}
			p.Records = append(p.Records, item)
			if len(p.Records) == limit {
				pos.Name, pos.Offset = segment.Name, offset
				b, _ := json.Marshal(pos)
				p.NextCursor = base64.RawURLEncoding.EncodeToString(b)
				_ = f.Close()
				return p, nil
			}
		}
		if err := f.Close(); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (a *Archive) requestPositions(q Query) (*os.Root, []segmentPosition, cursor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var pos cursor
	if q.ReadOnly && !a.readyLocked() {
		return nil, nil, pos, errNotReady
	}
	if a.root == nil {
		if _, err := os.Stat(a.dir); os.IsNotExist(err) && (a.enabled == nil || !a.enabled()) {
			return nil, nil, pos, nil
		}
	}
	if err := a.prepare(); err != nil {
		return nil, nil, pos, err
	}
	names, err := a.names("requests")
	if err != nil || len(names) == 0 {
		return nil, nil, pos, err
	}
	pos = cursor{Name: names[0], End: names[len(names)-1], Filter: queryFingerprint(q)}
	info, err := a.root.Stat(pos.End)
	if err != nil {
		return nil, nil, pos, err
	}
	pos.EndOffset = info.Size()
	if q.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(data, &pos) != nil || pos.Filter != queryFingerprint(q) || pos.Offset < 0 || pos.EndOffset < 0 || pos.Name > pos.End || !containsName(names, pos.Name) || !containsName(names, pos.End) {
			return nil, nil, pos, errors.New("invalid cursor")
		}
		endInfo, err := a.root.Stat(pos.End)
		if err != nil || pos.EndOffset > endInfo.Size() {
			return nil, nil, pos, errors.New("invalid cursor")
		}
	}
	segments := []segmentPosition{}
	for _, name := range names {
		if name < pos.Name || name > pos.End {
			continue
		}
		info, err := a.root.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			return nil, nil, pos, errors.New("unsafe history segment")
		}
		size := info.Size()
		if name == pos.End {
			size = pos.EndOffset
		}
		segments = append(segments, segmentPosition{Name: name, Size: size})
	}
	root, err := os.OpenRoot(a.dir)
	return root, segments, pos, err
}

// Bounds precede the potentially large UUID and aggregate maps in an index.
// Read only that small prefix, outside the writer lock. Missing or stale indexes
// conservatively fall back to the authoritative raw segment.
func requestSegmentInRange(root *os.Root, segment segmentPosition, q Query) bool {
	if q.From.IsZero() && q.To.IsZero() {
		return true
	}
	info, err := root.Lstat(segment.Name + ".index.json")
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	f, err := root.Open(segment.Name + ".index.json")
	if err != nil {
		return true
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4096))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return true
	}
	var from, to time.Time
	var size int64
	var haveSize, haveFrom, haveTo bool
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return true
		}
		switch key {
		case "size":
			err = d.Decode(&size)
			haveSize = true
		case "from":
			err = d.Decode(&from)
			haveFrom = true
		case "to":
			err = d.Decode(&to)
			haveTo = true
		default:
			var ignored json.RawMessage
			err = d.Decode(&ignored)
		}
		if err != nil {
			return true
		}
		if haveSize && haveFrom && haveTo {
			if size != segment.Size {
				return true
			}
			return (q.From.IsZero() || !to.Before(q.From)) && (q.To.IsZero() || from.Before(q.To))
		}
	}
	return true
}

func containsName(names []string, name string) bool {
	i := sort.SearchStrings(names, name)
	return i < len(names) && names[i] == name
}
func period(at time.Time, group string) string {
	switch group {
	case "year":
		return at.UTC().Format("2006")
	case "month":
		return at.UTC().Format("2006-01")
	default:
		return at.UTC().Format("2006-01-02")
	}
}
func totalKey(t Totals) string {
	return t.Period + "\x00" + t.Provider + "\x00" + t.Model + "\x00" + t.CredentialID
}
func addTotal(m map[string]Totals, r Request, group string) {
	t := Totals{Period: period(r.RequestedAt, group), Provider: r.Provider, Model: r.ExecutionModel, CredentialID: r.CredentialID}
	key := totalKey(t)
	if old, ok := m[key]; ok {
		t = old
	}
	t.Attempts++
	if r.Outcome != "success" {
		t.Failed++
	}
	b := r.Tokens
	t.InputTokens += b.Input.TotalTokens
	t.OutputTokens += b.Output.TotalTokens
	t.CacheReadTokens += b.Input.CacheReadTokens
	t.CacheWriteTokens += b.Input.CacheWriteTokens
	t.ReasoningTokens += b.Output.ReasoningTokens
	t.TotalTokens += b.TotalTokens
	if cacheMeasured(r) {
		t.MeasuredAttempts++
		t.MeasuredInput += b.Input.TotalTokens
		t.MeasuredCache += b.Input.CacheReadTokens
	}
	if t.MeasuredInput > 0 {
		p := 100 * float64(t.MeasuredCache) / float64(t.MeasuredInput)
		t.CachedInputPercent = &p
	}
	m[key] = t
}
func (a *Archive) Summary(q Query, group string) ([]Totals, error) {
	return a.SummaryContext(context.Background(), q, group)
}

func (a *Archive) SummaryContext(ctx context.Context, q Query, group string) ([]Totals, error) {
	if group != "day" && group != "month" && group != "year" {
		return nil, errors.New("group must be day, month or year")
	}
	// Raw iteration also handles partial UTC days and aliases correctly. Pages and
	// segment indexes remain bounded; only the requested summary groups accumulate.
	totals := map[string]Totals{}
	q.Limit = 500
	q.Cursor = ""
	for {
		p, err := a.RequestsContext(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, r := range p.Records {
			addTotal(totals, r, group)
		}
		if p.NextCursor == "" {
			break
		}
		q.Cursor = p.NextCursor
	}
	out := make([]Totals, 0, len(totals))
	for _, t := range totals {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return totalKey(out[i]) < totalKey(out[j]) })
	return out, nil
}

// Allowance visits at most a caller-selected 31 day interval. A visitor permits
// streaming graphs with dense observations without an unbounded response buffer.
func (a *Archive) Allowance(q Query, duration int64, visit func(Observation) error) error {
	if q.CredentialID == "" || q.From.IsZero() || q.To.IsZero() || !q.To.After(q.From) || q.To.Sub(q.From) > 31*24*time.Hour {
		return errors.New("one credential and a range of at most 31 days required")
	}
	if duration != 604800 && duration != 18000 {
		return errors.New("unsupported quota window")
	}
	if a == nil {
		return errors.New("history unavailable")
	}
	root, segments, err := a.allowancePositions(q)
	if err != nil {
		return err
	}
	if root == nil {
		return nil
	}
	defer root.Close()
	for _, segment := range segments {
		info, e := root.Lstat(segment.Name)
		if e != nil || !info.Mode().IsRegular() {
			return errors.New("unsafe history segment")
		}
		f, e := root.Open(segment.Name)
		if e != nil {
			return e
		}
		reader := bufio.NewReader(io.LimitReader(f, segment.Size))
		for {
			line, e := reader.ReadBytes('\n')
			if e == io.EOF {
				break
			}
			if e != nil {
				_ = f.Close()
				return e
			}
			var o Observation
			if json.Unmarshal(line, &o) != nil {
				continue
			}
			if o.DurationSeconds == duration && matches(q, o.ObservedAt, o.Provider, o.Model, o.CredentialID) {
				if e = visit(o); e != nil {
					_ = f.Close()
					return e
				}
			}
		}
		if e = f.Close(); e != nil {
			return e
		}
	}

	return nil
}

// Export streams stable archive pages. The visitor must not mutate the archive.
func (a *Archive) Export(q Query, visit func(Request) error) error {
	return a.ExportContext(context.Background(), q, visit)
}

func (a *Archive) ExportContext(ctx context.Context, q Query, visit func(Request) error) error {
	q.Limit = 500
	q.Cursor = ""
	for {
		p, err := a.RequestsContext(ctx, q)
		if err != nil {
			return err
		}
		for _, r := range p.Records {
			if err = visit(r); err != nil {
				return err
			}
		}
		if p.NextCursor == "" {
			return nil
		}
		q.Cursor = p.NextCursor
	}
}

var errNotReady = errors.New("history archive not ready for read-only queries")

// readyLocked reports whether prepare would do nothing: the archive is open,
// not closed, and has no pending legacy import.
func (a *Archive) readyLocked() bool {
	pendingImport := a.legacyDir != "" && !a.importComplete && a.enabled != nil && a.enabled()
	return a.root != nil && !a.closed && !pendingImport
}

// PublicObservation omits the internal logical-account continuity reference.
func PublicObservation(o Observation) Observation { o.IdentityRef = ""; return o }

type segmentPosition struct {
	Name string
	Size int64
}

// Hold the writer lock only while capturing immutable, synced byte positions.
// The separate read root survives an orderly archive close and cannot block the
// writer through a slow HTTP visitor.
func (a *Archive) allowancePositions(q Query) (*os.Root, []segmentPosition, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if q.ReadOnly && !a.readyLocked() {
		return nil, nil, errNotReady
	}
	if a.root == nil {
		if _, err := os.Stat(a.dir); os.IsNotExist(err) && (a.enabled == nil || !a.enabled()) {
			return nil, nil, nil
		}
	}
	if err := a.prepare(); err != nil {
		return nil, nil, err
	}
	names, err := a.names("quota")
	if err != nil {
		return nil, nil, err
	}
	positions := []segmentPosition{}
	for _, name := range names {
		idx, e := a.index(name)
		if e != nil {
			return nil, nil, e
		}
		a.noteIndex(name, idx)
		if !idx.To.Before(q.From) && idx.From.Before(q.To) {
			positions = append(positions, segmentPosition{Name: name, Size: idx.Size})
		}
	}
	root, err := os.OpenRoot(a.dir)
	return root, positions, err
}
