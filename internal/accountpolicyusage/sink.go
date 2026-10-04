// Package accountpolicyusage persists sanitized request measurements through the
// supported SDK usage plugin interface. Token counts describe inference work;
// they are not a measurement of subscription allowance or cache lifetime.
package accountpolicyusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var segmentNames = [...]string{"usage.jsonl", "usage.1.jsonl", "usage.2.jsonl"}

// Sink is a bounded JSONL sink. The SDK usage manager calls it on its dispatcher,
// away from inference. Each accepted completion is synced before returning.
// Only one sink should own a state directory in a running deployment.
type Sink struct {
	mu                sync.Mutex
	dir               string
	enabled           func() bool
	root              *os.Root
	closed            bool
	maxFileBytes      int64
	ids               [3]map[string]struct{}
	sizes             [3]int64
	counts            [3]uint64
	status            Status
	dashboardLoaded   bool
	dashboardValid    bool
	dashboardSegments [3]map[int64]*dashboardBucket
}

// Status reports retained completions and runtime write health, without secrets.
// Written, Duplicates, and WriteErrors count this sink instance only.
type Status struct {
	Retained    uint64    `json:"retained"`
	Written     uint64    `json:"written"`
	Duplicates  uint64    `json:"duplicates"`
	WriteErrors uint64    `json:"write_errors"`
	LastWriteAt time.Time `json:"last_write_at"`
	Closed      bool      `json:"closed"`
}

// event is deliberately an allowlist, not an embedded or copied usage.Record.
// SDK zero values cannot establish whether cache metrics were reported. Only
// positive measured cache values and TTFT are stored; missing values stay null.
type event struct {
	SchemaVersion    int       `json:"schema_version"`
	RequestID        string    `json:"request_id"`
	Provider         string    `json:"provider"`
	CredentialID     string    `json:"credential_id"`
	Model            string    `json:"model"`
	RequestedAt      time.Time `json:"requested_at"`
	LatencyMS        float64   `json:"latency_ms"`
	TTFTMS           *float64  `json:"ttft_ms"`
	Failed           bool      `json:"failed"`
	StatusCode       int       `json:"status_code"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	CacheReadTokens  *int64    `json:"cache_read_tokens"`
	CacheWriteTokens *int64    `json:"cache_write_tokens"`
}

var _ usage.Plugin = (*Sink)(nil)

// New resolves the state directory without creating or modifying anything.
// An enabled sink validates directory safety immediately; a disabled sink defers
// that validation until its first enabled completion. A nil callback keeps the
// sink disabled, preserving existing startup behavior for optional collection.
func New(dir string, enabled func() bool) (*Sink, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("usage sink state directory is required")
	}
	abs, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		return nil, fmt.Errorf("resolve usage state directory: %w", errAbs)
	}
	if enabled != nil && enabled() {
		if errCheck := checkDirectory(abs); errCheck != nil {
			return nil, errCheck
		}
	}
	return &Sink{dir: abs, enabled: enabled, maxFileBytes: 10 * 1024 * 1024}, nil
}

func checkDirectory(dir string) error {
	for {
		info, errStat := os.Lstat(dir)
		if errStat != nil && !os.IsNotExist(errStat) {
			return fmt.Errorf("inspect usage state directory: %w", errStat)
		}
		if errStat == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("usage state directory must contain only real directories")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}

// HandleUsage appends exactly one sanitized event for an execution UUID. Request
// IDs retained in the three segments are deduplicated across restarts. An invalid
// SDK request ID is replaced with a fresh UUID, never copied into storage.
func (s *Sink) HandleUsage(_ context.Context, record usage.Record) {
	if s == nil || s.enabled == nil || !s.enabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if errWrite := s.append(record); errWrite != nil {
		s.status.WriteErrors++
		// Do not log record contents, directory paths, or underlying errors, which
		// can include credential-derived filenames or other sensitive metadata.
		log.WithField("component", "account-policy-usage").Error("durable usage write failed")
		if s.root != nil {
			if errClose := s.root.Close(); errClose != nil {
				log.WithField("component", "account-policy-usage").Error("usage directory close failed")
			}
			s.root = nil
		}
	}
}

func (s *Sink) append(record usage.Record) error {
	if s.root == nil {
		if errInit := s.initialize(); errInit != nil {
			return errInit
		}
	}
	item := allowlisted(record)
	for _, ids := range s.ids {
		if _, exists := ids[item.RequestID]; exists {
			s.status.Duplicates++
			return nil
		}
	}
	data, errMarshal := json.Marshal(item)
	if errMarshal != nil {
		return fmt.Errorf("encode usage event: %w", errMarshal)
	}
	data = append(data, '\n')
	if int64(len(data)) > s.maxFileBytes {
		return errors.New("usage event exceeds segment size")
	}
	if s.sizes[0]+int64(len(data)) > s.maxFileBytes {
		if errRotate := s.rotate(); errRotate != nil {
			return errRotate
		}
	}
	file, errOpen := s.openSegment(segmentNames[0], os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if errOpen != nil {
		return errOpen
	}
	_, errWrite := file.Write(data)
	errSync := file.Sync()
	errClose := file.Close()
	if err := errors.Join(errWrite, errSync, errClose); err != nil {
		return fmt.Errorf("persist usage completion: %w", err)
	}
	if errSync := s.syncDirectory(); errSync != nil {
		return errSync
	}
	s.ids[0][item.RequestID] = struct{}{}
	s.sizes[0] += int64(len(data))
	s.counts[0]++
	s.status.Written++
	s.status.LastWriteAt = time.Now().UTC()
	if s.dashboardLoaded && s.dashboardValid {
		s.addDashboardEvent(0, item)
	}
	return nil
}

func allowlisted(record usage.Record) event {
	id, errUUID := uuid.Parse(record.RequestID)
	if errUUID != nil || id == uuid.Nil {
		id = uuid.New()
	}
	model := record.Alias
	if model == "" {
		model = record.Model
	}
	item := event{SchemaVersion: 1, RequestID: id.String(), Provider: record.Provider, CredentialID: record.AuthID,
		Model: model, RequestedAt: record.RequestedAt.UTC(), LatencyMS: float64(record.Latency) / float64(time.Millisecond),
		Failed: record.Failed, StatusCode: record.Fail.StatusCode, InputTokens: record.Detail.InputTokens,
		OutputTokens: record.Detail.OutputTokens, ReasoningTokens: record.Detail.ReasoningTokens, TotalTokens: record.Detail.TotalTokens}
	cacheRead := record.Detail.CacheReadTokens
	canonical := record.Detail.TokenBreakdown
	if cacheRead == 0 && (!canonical.Valid() || canonical.Quality != usage.TokenAccountingQualityComplete) {
		cacheRead = record.Detail.CachedTokens
	}
	if cacheRead > 0 {
		item.CacheReadTokens = &cacheRead
	}
	if record.Detail.CacheCreationTokens > 0 {
		cacheWrite := record.Detail.CacheCreationTokens
		item.CacheWriteTokens = &cacheWrite
	}
	if record.TTFT > 0 {
		ttft := float64(record.TTFT) / float64(time.Millisecond)
		item.TTFTMS = &ttft
	}
	return item
}

func (s *Sink) initialize() error {
	// Recovery can truncate unfinished tails, so rebuild the read cache after it.
	s.dashboardLoaded, s.dashboardValid = false, false
	s.dashboardSegments = [3]map[int64]*dashboardBucket{}
	if errCheck := checkDirectory(s.dir); errCheck != nil {
		return errCheck
	}
	if errMkdir := os.MkdirAll(s.dir, 0700); errMkdir != nil {
		return fmt.Errorf("create usage state directory: %w", errMkdir)
	}
	root, errOpen := os.OpenRoot(s.dir)
	if errOpen != nil {
		return fmt.Errorf("open usage state directory: %w", errOpen)
	}
	s.root = root
	dir, errDir := root.Open(".")
	if errDir != nil {
		return fmt.Errorf("open usage directory permissions: %w", errDir)
	}
	errChmod := dir.Chmod(0700)
	errClose := dir.Close()
	if err := errors.Join(errChmod, errClose); err != nil {
		return fmt.Errorf("protect usage state directory: %w", err)
	}
	for i, name := range segmentNames {
		s.ids[i] = make(map[string]struct{})
		s.sizes[i], s.counts[i] = 0, 0
		info, errStat := root.Lstat(name)
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil {
			return fmt.Errorf("inspect usage segment: %w", errStat)
		}
		if !info.Mode().IsRegular() || info.Size() > s.maxFileBytes {
			return errors.New("usage segment must be a bounded regular file")
		}
		file, errFile := s.openSegment(name, os.O_RDWR)
		if errFile != nil {
			return errFile
		}
		data := make([]byte, info.Size())
		_, errRead := file.ReadAt(data, 0)
		if len(data) == 0 {
			errRead = nil
		}
		// A crash can leave an unfinished final line. Recover only that tail;
		// malformed complete events cause a visible error instead of data loss.
		end := bytes.LastIndexByte(data, '\n') + 1
		var errTruncate, errSync error
		if errRead == nil && end != len(data) {
			errTruncate = file.Truncate(int64(end))
			errSync = file.Sync()
			data = data[:end]
		}
		errClose := file.Close()
		if err := errors.Join(errRead, errTruncate, errSync, errClose); err != nil {
			return fmt.Errorf("recover usage segment: %w", err)
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var saved event
			if errDecode := json.Unmarshal(line, &saved); errDecode != nil {
				return errors.New("invalid complete usage event")
			}
			if id, errUUID := uuid.Parse(saved.RequestID); errUUID != nil || id == uuid.Nil {
				return errors.New("invalid persisted usage request ID")
			}
			s.ids[i][saved.RequestID] = struct{}{}
			s.counts[i]++
		}
		s.sizes[i] = int64(len(data))
	}
	return nil
}

func (s *Sink) openSegment(name string, flags int) (*os.File, error) {
	info, errStat := s.root.Lstat(name)
	if errStat != nil && !os.IsNotExist(errStat) {
		return nil, fmt.Errorf("inspect usage segment: %w", errStat)
	}
	if errStat == nil && !info.Mode().IsRegular() {
		return nil, errors.New("usage segment must be a regular file")
	}
	file, errOpen := s.root.OpenFile(name, flags, 0600)
	if errOpen != nil {
		return nil, fmt.Errorf("open confined usage segment: %w", errOpen)
	}
	if errChmod := file.Chmod(0600); errChmod != nil {
		errClose := file.Close()
		return nil, fmt.Errorf("protect usage segment: %w", errors.Join(errChmod, errClose))
	}
	return file, nil
}

func (s *Sink) rotate() error {
	// Fixed filenames and os.Root keep all operations inside the state directory,
	// including when a path is replaced by a symlink concurrently.
	for i := len(segmentNames) - 1; i >= 0; i-- {
		info, errStat := s.root.Lstat(segmentNames[i])
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil || !info.Mode().IsRegular() {
			return errors.New("cannot rotate nonregular usage segment")
		}
		if i == len(segmentNames)-1 {
			if errRemove := s.root.Remove(segmentNames[i]); errRemove != nil {
				return fmt.Errorf("remove oldest usage segment: %w", errRemove)
			}
		} else if errRename := s.root.Rename(segmentNames[i], segmentNames[i+1]); errRename != nil {
			return fmt.Errorf("rotate usage segment: %w", errRename)
		}
	}
	for i := len(segmentNames) - 1; i > 0; i-- {
		s.ids[i], s.sizes[i], s.counts[i] = s.ids[i-1], s.sizes[i-1], s.counts[i-1]
		s.dashboardSegments[i] = s.dashboardSegments[i-1]
	}
	s.ids[0], s.sizes[0], s.counts[0] = make(map[string]struct{}), 0, 0
	s.dashboardSegments[0] = nil
	return s.syncDirectory()
}

func (s *Sink) syncDirectory() error {
	dir, errOpen := s.root.Open(".")
	if errOpen != nil {
		return fmt.Errorf("open usage directory for sync: %w", errOpen)
	}
	var errSync error
	// Windows does not support flushing directory handles. Completion files
	// themselves are still synced on every write on all platforms.
	if runtime.GOOS != "windows" {
		errSync = dir.Sync()
	}
	errClose := dir.Close()
	if err := errors.Join(errSync, errClose); err != nil {
		return fmt.Errorf("sync usage directory: %w", err)
	}
	return nil
}

// Summary returns aggregate sink health, never an SDK record or request body.
func (s *Sink) Summary() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	status.Retained = s.counts[0] + s.counts[1] + s.counts[2]
	status.Closed = s.closed
	return status
}

// Close is idempotent. Completions are already synced; shutdown only closes the
// directory handle. Lifecycle owners should drain the usage manager first.
func (s *Sink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.root != nil {
		if errClose := s.root.Close(); errClose != nil {
			return fmt.Errorf("close usage state directory: %w", errClose)
		}
		s.root = nil
	}
	return nil
}
