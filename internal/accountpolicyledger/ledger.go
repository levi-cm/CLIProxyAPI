package accountpolicyledger

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const segmentLimit = 16 * 1024 * 1024

type segmentIndex struct {
	Version int               `json:"version"`
	Size    int64             `json:"size"`
	Records uint64            `json:"records"`
	From    time.Time         `json:"from"`
	To      time.Time         `json:"to"`
	Offsets []int64           `json:"sparse_offsets"`
	IDs     map[string]bool   `json:"execution_ids"`
	Daily   map[string]Totals `json:"daily"`
	Healthy bool              `json:"healthy"`
}

// Archive serializes appenders independently of the router and policy locks.
// Queries retain at most one segment index and a bounded page of raw records.
type Archive struct {
	lookupReady    bool
	active         map[string]string
	legacyDir      string
	importComplete bool
	inFlight       string
	recovery       map[string]bool
	owner          *os.File
	cache          map[string]segmentIndex
	// lookupMemo caches parsed lookup leaves and split markers during startup
	// repair and bulk legacy imports; nil otherwise. Any lookup write clears it.
	lookupMemo      *lookupMemo
	mu              sync.Mutex
	dir             string
	enabled         func() bool
	root            *os.Root
	maxSegmentBytes int64
	status          Status
	closed          bool
}

func checkDirectory(dir string) error {
	for {
		info, err := os.Lstat(dir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("history requires real directories")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func New(stateDir string, enabled func() bool) (*Archive, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("history state directory required")
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	a := &Archive{dir: filepath.Join(abs, "history"), enabled: enabled, maxSegmentBytes: segmentLimit, status: Status{SchemaVersion: 1, Healthy: true, UnhealthySegments: []string{}, UncertainCoverage: []CoverageGap{}, CoverageNote: "Provider attempts, not unique client requests. Missing upstream measurements, pre-adoption history and abrupt loss before dispatch are not recoverable."}}
	if err = checkDirectory(a.dir); err != nil {
		return nil, err
	}
	if enabled != nil && enabled() {
		if err = a.initialize(); err != nil {
			if a.owner != nil {
				_ = a.owner.Close()
			}
			if a.root != nil {
				_ = a.root.Close()
			}
			return nil, err
		}
	}
	return a, nil
}

func (a *Archive) initialize() error {
	if a.root != nil {
		return nil
	}
	if err := checkDirectory(a.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(a.dir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(a.dir)
	if err != nil {
		return err
	}
	a.root = root
	owner, err := a.open("writer.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		_ = root.Close()
		a.root = nil
		return err
	}
	if err = lockArchive(owner); err != nil {
		_ = owner.Close()
		_ = root.Close()
		a.root = nil
		return errors.New("history already has an authoritative writer")
	}
	a.owner = owner
	a.cache = map[string]segmentIndex{}
	a.active = map[string]string{}
	a.recovery = map[string]bool{}
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	err = errors.Join(d.Chmod(0700), d.Close())
	if err != nil {
		return err
	}
	// Status is best effort; raw segments remain the authority after a crash.
	if f, e := a.open("collection-status.json", os.O_RDONLY); e == nil {
		var saved Status
		if json.NewDecoder(f).Decode(&saved) == nil {
			a.status = saved
			a.status.Closed = false
			a.status.Written = 0
			a.status.Duplicates = 0
			if !saved.Closed {
				from := saved.LastWriteAt
				if from.IsZero() {
					from = saved.ProcessStartedAt
				}
				a.status.UncertainCoverage = append(a.status.UncertainCoverage, CoverageGap{From: from, To: time.Now().UTC(), Reason: "previous_process_did_not_close_cleanly"})
			}
		}
		_ = f.Close()
	}
	for _, kind := range []string{"requests", "quota"} {
		names, e := a.names(kind)
		if e != nil {
			return e
		}
		if len(names) > 0 {
			a.active[kind] = names[len(names)-1]
		}
		var expected lookupSignature
		for _, name := range names {
			idx, e := a.index(name)
			if e != nil {
				return e
			}
			a.noteIndex(name, idx)
			for id := range idx.IDs {
				expected.add(lookupDigest(id), name)
			}
		}
		actual, lookupErr := a.lookupSignature(kind, "")
		if lookupErr == nil && actual != expected && a.repairLookup(kind, names) == nil {
			actual, lookupErr = a.lookupSignature(kind, "")
		}
		if lookupErr != nil || actual != expected {
			if err := a.restoreLookup(kind, names); err != nil {
				return err
			}
		}
	}
	a.lookupReady = true
	a.status.Closed = false
	a.status.ProcessStartedAt = time.Now().UTC()
	a.noteCollectionEnabled(a.enabled != nil && a.enabled())
	return a.saveJSON("collection-status.json", a.status)
}

// SetCollectionEnabled checkpoints changes independently of traffic, including
// an interval with no completions before collection is enabled again.
func (a *Archive) SetCollectionEnabled(enabled bool) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.New("history closed")
	}
	if a.root == nil && !enabled {
		if _, err := os.Stat(a.dir); os.IsNotExist(err) {
			return nil
		}
	}
	if err := a.initialize(); err != nil {
		a.writeError()
		return err
	}
	a.noteCollectionEnabled(enabled)
	if err := a.saveJSON("collection-status.json", a.status); err != nil {
		a.writeError()
		return err
	}
	return nil
}

func (a *Archive) noteCollectionEnabled(enabled bool) {
	now := time.Now().UTC()
	for i := range a.status.UncertainCoverage {
		gap := &a.status.UncertainCoverage[i]
		if gap.Reason == "collection_disabled" && gap.To.IsZero() {
			if enabled {
				gap.To = now
			}
			return
		}
	}
	if !enabled {
		a.status.UncertainCoverage = append(a.status.UncertainCoverage, CoverageGap{From: now, Reason: "collection_disabled"})
	}
}

func (a *Archive) open(name string, flags int) (*os.File, error) {
	if filepath.Base(name) != name {
		return nil, errors.New("unsafe archive name")
	}
	info, err := a.root.Lstat(name)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("archive file must be regular")
	}
	f, err := a.root.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
func (a *Archive) names(kind string) ([]string, error) {
	d, err := a.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	err = errors.Join(err, d.Close())
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, kind+"-") && strings.HasSuffix(n, ".jsonl") {
			if !e.Type().IsRegular() {
				return nil, errors.New("nonregular history segment")
			}
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}
func (a *Archive) noteIndex(name string, idx segmentIndex) {
	if !idx.From.IsZero() && (a.status.CoverageStart.IsZero() || idx.From.Before(a.status.CoverageStart)) {
		a.status.CoverageStart = idx.From
	}
	if !idx.Healthy {
		a.status.Healthy = false
		for _, s := range a.status.UnhealthySegments {
			if s == name {
				return
			}
		}
		a.status.UnhealthySegments = append(a.status.UnhealthySegments, name)
	}
}
func (a *Archive) index(name string) (segmentIndex, error) {
	if cached, ok := a.cache[name]; ok {
		for _, bad := range a.status.UnhealthySegments {
			if bad == name {
				cached.Healthy = false
			}
		}
		return cached, nil
	}
	info, err := a.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return segmentIndex{}, errors.New("unsafe history segment")
	}
	if f, e := a.open(name+".index.json", os.O_RDONLY); e == nil {
		var idx segmentIndex
		e = json.NewDecoder(io.LimitReader(f, 64*1024*1024)).Decode(&idx)
		_ = f.Close()
		if e == nil && idx.Version == 3 && idx.Size == info.Size() && idx.IDs != nil && idx.Daily != nil && idx.Offsets != nil {
			for _, bad := range a.status.UnhealthySegments {
				if bad == name {
					idx.Healthy = false
				}
			}
			return idx, nil
		}
	}
	return a.rebuild(name, info.Size())
}
func (a *Archive) rebuild(name string, size int64) (segmentIndex, error) {
	idx := segmentIndex{Version: 3, Size: size, Healthy: true, IDs: map[string]bool{}, Daily: map[string]Totals{}, Offsets: []int64{}}
	f, err := a.open(name, os.O_RDONLY)
	if err != nil {
		return idx, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var offset int64
	for {
		line, e := r.ReadBytes('\n')
		if len(line) > segmentLimit {
			return idx, errors.New("oversize archive record")
		}
		if e == io.EOF {
			if len(line) > 0 {
				idx.Healthy = false
			}
			break
		}
		if e != nil {
			return idx, e
		}
		at := time.Time{}
		id := ""
		valid := true
		if strings.HasPrefix(name, "requests-") {
			var item Request
			if json.Unmarshal(line, &item) != nil || item.SchemaVersion != 1 || item.RequestID == "" {
				valid = false
			} else {
				at, id = item.RequestedAt, item.RequestID
				addTotal(idx.Daily, item, "day")
			}
		} else {
			var item Observation
			if json.Unmarshal(line, &item) != nil || item.SchemaVersion != 1 || item.ID == "" {
				valid = false
			} else {
				at, id = item.ObservedAt, item.ID
			}
		}
		if valid {
			if idx.Records%128 == 0 {
				idx.Offsets = append(idx.Offsets, offset)
			}
			idx.Records++
			idx.IDs[id] = true
			if a.lookupReady {
				if e := a.markID(strings.SplitN(name, "-", 2)[0], id, name); e != nil {
					return idx, e
				}
			}
			if idx.From.IsZero() || at.Before(idx.From) {
				idx.From = at
			}
			if at.After(idx.To) {
				idx.To = at
			}
		} else {
			idx.Healthy = false
		}
		offset += int64(len(line))
	}
	if err = a.saveJSON(name+".index.json", idx); err != nil {
		return idx, err
	}
	return idx, nil
}
func (a *Archive) saveJSON(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// Fixed confined names; never follow a symlink when replacing an index.
	if info, e := a.root.Lstat(name); e == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe archive metadata")
	}
	tmp := name + ".tmp"
	f, err := a.open(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = a.root.Rename(tmp, name); err != nil {
		return err
	}
	return a.syncDir()
}
func (a *Archive) syncDir() error {
	d, err := a.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (a *Archive) append(kind, id string, v any) error {
	if err := a.initialize(); err != nil {
		return err
	}
	var err error
	for name := range a.recovery {
		info, e := a.root.Lstat(name)
		if e != nil {
			return e
		}
		if _, e = a.rebuild(name, info.Size()); e != nil {
			return e
		}
		delete(a.recovery, name)
	}
	if exists, e := a.hasID(kind, id); e != nil {
		return e
	} else if exists {
		a.status.Duplicates++
		return nil
	}
	var idx segmentIndex
	if a.active[kind] != "" {
		idx, err = a.index(a.active[kind])
		if err != nil {
			return err
		}
		a.noteIndex(a.active[kind], idx)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > int(a.maxSegmentBytes) {
		return errors.New("history record exceeds segment limit")
	}
	name := a.active[kind]
	if name == "" || idx.Size+int64(len(data)) > a.maxSegmentBytes || !idx.Healthy {
		seq := uint64(1)
		if name != "" {
			if _, err = fmt.Sscanf(name, kind+"-%020d.jsonl", &seq); err != nil {
				return err
			}
			seq++
		}
		for cachedName, cached := range a.cache {
			if strings.HasPrefix(cachedName, kind+"-") {
				if e := a.saveJSON(cachedName+".index.json", cached); e != nil {
					return e
				}
				delete(a.cache, cachedName)
			}
		}
		idx = segmentIndex{Version: 3, Healthy: true, IDs: map[string]bool{}, Daily: map[string]Totals{}, Offsets: []int64{}}
		name = fmt.Sprintf("%s-%020d.jsonl", kind, seq)
		f, e := a.open(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if e != nil {
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
	}
	a.active[kind] = name
	a.inFlight = name
	f, err := a.open(name, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = a.syncDir(); err != nil {
		return err
	}
	if idx.Records%128 == 0 {
		idx.Offsets = append(idx.Offsets, idx.Size)
	}
	idx.Size += int64(len(data))
	idx.Records++
	idx.IDs[id] = true
	var at time.Time
	switch item := v.(type) {
	case Request:
		at = item.RequestedAt
		addTotal(idx.Daily, item, "day")
	case Observation:
		at = item.ObservedAt
	}
	if idx.From.IsZero() || at.Before(idx.From) {
		idx.From = at
	}
	if at.After(idx.To) {
		idx.To = at
	}
	a.cache[name] = idx
	if err = a.markID(kind, id, name); err != nil {
		return err
	}
	a.inFlight = ""
	a.noteIndex(name, idx)
	// A stale index is safe: restart detects the size change and rebuilds.
	a.status.Written++
	a.status.LastWriteAt = time.Now().UTC()
	return nil
}
func (a *Archive) writeError() {
	if a.inFlight != "" {
		name := a.inFlight
		delete(a.cache, name)
		a.recovery[name] = true
		found := false
		for _, n := range a.status.UnhealthySegments {
			if n == name {
				found = true
			}
		}
		if !found {
			a.status.UnhealthySegments = append(a.status.UnhealthySegments, name)
		}
		a.inFlight = ""
	}
	a.status.WriteErrors++
	a.status.Healthy = false
	now := time.Now().UTC()
	a.status.UncertainCoverage = append(a.status.UncertainCoverage, CoverageGap{From: now, To: now, Reason: "archive_write_failure"})
	if a.root != nil {
		_ = a.saveJSON("collection-status.json", a.status)
	}
}

func (a *Archive) HandleUsage(_ context.Context, r usage.Record) {
	if a == nil || a.enabled == nil || !a.enabled() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	if err := a.prepare(); err != nil {
		a.writeError()
		return
	}
	item := Allowlisted(r)
	if err := a.append("requests", item.RequestID, item); err != nil {
		a.writeError()
	}
}
func Allowlisted(r usage.Record) Request {
	id, err := uuid.Parse(r.RequestID)
	if err != nil || id == uuid.Nil {
		id = uuid.New()
	}
	if r.ExecutionModel != "" {
		r.Model = r.ExecutionModel
	}
	d := usage.EnsureTokenBreakdownForProvider(r.Detail, r.Provider, r.ExecutorType)
	if r.ResponseStatusCode > 0 {
		r.Fail.StatusCode = r.ResponseStatusCode
	}
	outcome := "success"
	if r.Failed {
		outcome = "failed"
	}
	item := Request{SchemaVersion: 1, RequestID: id.String(), ParentExecutionID: r.ParentExecutionID, TraceID: r.TraceID, Provider: r.Provider, CredentialID: r.AuthID, RequestedModel: r.Alias, ExecutionModel: r.Model, ResponseModel: r.ResponseModel, RequestedAt: r.RequestedAt.UTC(), DurationMS: float64(r.Latency) / float64(time.Millisecond), Stream: r.Stream, Outcome: outcome, StatusCode: r.Fail.StatusCode, Generate: usage.GenerateEnabled(r.Generate), AuthCategory: r.AuthType, RequestedServiceTier: r.ServiceTier, EffectiveServiceTier: r.EffectiveServiceTier, ResponseServiceTier: r.ResponseServiceTier, Tokens: d.TokenBreakdown, Measurements: d.Measurements}
	item.MeasurementQuality = "unknown"
	if d.Measurements.Input || d.Measurements.Output || d.Measurements.Total {
		item.MeasurementQuality = "incomplete"
	}
	if d.Measurements.Input && d.Measurements.Output && d.TokenBreakdown.Quality == usage.TokenAccountingQualityComplete {
		item.MeasurementQuality = "complete"
	}
	if d.TokenBreakdown.Quality == usage.TokenAccountingQualityInconsistent {
		item.MeasurementQuality = "inconsistent"
	}
	if item.ResponseServiceTier == "" {
		item.ResponseServiceTier = d.ResponseServiceTier
	}
	if item.Measurements.CacheWrite5m {
		v := d.CacheWrite5mTokens
		item.CacheWrite5m = &v
	}
	if item.Measurements.CacheWrite1h {
		v := d.CacheWrite1hTokens
		item.CacheWrite1h = &v
	}
	if cacheMeasured(item) && item.Tokens.Input.TotalTokens > 0 {
		p := 100 * float64(item.Tokens.Input.CacheReadTokens) / float64(item.Tokens.Input.TotalTokens)
		item.CachedInputPercent = &p
	}
	return item
}
func cacheMeasured(r Request) bool {
	return r.Measurements.Input && r.Measurements.CacheRead && r.Tokens.Valid() && r.Tokens.Quality == usage.TokenAccountingQualityComplete
}
func (a *Archive) Observe(o Observation) error {
	if a == nil || a.enabled == nil || !a.enabled() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.New("history closed")
	}
	if err := a.prepare(); err != nil {
		a.writeError()
		return err
	}
	o.SchemaVersion = 1
	if o.ID == "" {
		o.ID = uuid.NewString()
	}
	if err := a.append("quota", o.ID, o); err != nil {
		a.writeError()
		return err
	}
	return nil
}
func (a *Archive) Status() Status {
	if a == nil {
		return Status{SchemaVersion: 1, Healthy: false, CoverageNote: "History unavailable"}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.status
	s.Enabled = a.enabled != nil && a.enabled()
	s.Closed = a.closed
	s.UnhealthySegments = append([]string{}, s.UnhealthySegments...)
	s.UncertainCoverage = append([]CoverageGap{}, s.UncertainCoverage...)
	return s
}
func (a *Archive) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	a.status.Closed = true
	if a.root != nil {
		var err error
		for name, idx := range a.cache {
			err = errors.Join(err, a.saveJSON(name+".index.json", idx))
		}
		err = errors.Join(err, a.saveJSON("collection-status.json", a.status))
		if a.owner != nil {
			err = errors.Join(err, a.owner.Close())
			a.owner = nil
		}
		err = errors.Join(err, a.root.Close())
		a.root = nil
		return err
	}
	return nil
}
