package accountpolicyledger

import (
	"context"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestArchiveHasSingleWriterAndSyncedPermissions(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if b, err := New(dir, func() bool { return true }); err == nil {
		b.Close()
		t.Fatal("second writer accepted")
	}
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: time.Now()})
	files, _ := filepath.Glob(filepath.Join(dir, "history", "*"))
	for _, path := range files {
		info, _ := os.Stat(path)
		expected := os.FileMode(0600)
		if info.IsDir() {
			expected = 0700
		}
		if info.Mode().Perm() != expected {
			t.Fatalf("unsafe permissions %v", info.Mode())
		}
	}
	info, _ := os.Stat(filepath.Join(dir, "history"))
	if info.Mode().Perm() != 0700 {
		t.Fatal("unsafe directory")
	}
}

type pausedQueryContext struct {
	context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *pausedQueryContext) Err() error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Context.Err()
}

func TestRequestScanDoesNotBlockArchiveWriter(t *testing.T) {
	a, err := New(t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Model: "recorded", RequestedAt: time.Now()})
	ctx := &pausedQueryContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	readDone := make(chan error, 1)
	go func() { _, err := a.RequestsContext(ctx, Query{Model: "absent", Limit: 1}); readDone <- err }()
	<-ctx.entered
	writeDone := make(chan struct{})
	go func() {
		a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: time.Now()})
		close(writeDone)
	}()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		close(ctx.release)
		<-readDone
		t.Fatal("historical request scan blocked collection")
	}
	close(ctx.release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.RequestsContext(cancelled, Query{Model: "absent"}); err != context.Canceled {
		t.Fatalf("cancelled query: %v", err)
	}
}

func TestCollectionDisabledCoverageSurvivesRestart(t *testing.T) {
	enabled := true
	dir := t.TempDir()
	a, err := New(dir, func() bool { return enabled })
	if err != nil {
		t.Fatal(err)
	}
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: time.Now()})
	enabled = false
	if err := a.SetCollectionEnabled(false); err != nil {
		t.Fatal(err)
	}
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: time.Now()})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	enabled = true
	a, err = New(dir, func() bool { return enabled })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	gaps := a.Status().UncertainCoverage
	if len(gaps) != 1 || gaps[0].Reason != "collection_disabled" || gaps[0].From.IsZero() || gaps[0].To.Before(gaps[0].From) {
		t.Fatalf("disabled coverage interval missing: %+v", gaps)
	}
	page, err := a.Requests(Query{})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("disabled records archived: %+v %v", page, err)
	}
}

func TestSlowAllowanceVisitorDoesNotBlockArchiveWriter(t *testing.T) {
	a, _ := New(t.TempDir(), func() bool { return true })
	defer a.Close()
	now := time.Now().UTC()
	if err := a.Observe(Observation{ID: "fixture-observation", CredentialID: "fixture", ObservedAt: now, DurationSeconds: 604800}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- a.Allowance(Query{CredentialID: "fixture", From: now.Add(-time.Hour), To: now.Add(time.Hour)}, 604800, func(Observation) error { close(entered); <-release; return nil })
	}()
	<-entered
	writeDone := make(chan struct{})
	go func() {
		a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: now})
		close(writeDone)
	}()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		close(release)
		<-readDone
		t.Fatal("slow reader blocked archive writer")
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
}
func TestRunningMarkerMakesAbruptExitVisible(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(dir, func() bool { return true })
	raw, err := os.ReadFile(filepath.Join(dir, "history", "collection-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"closed":true`) {
		t.Fatal("running process persisted clean marker")
	}
	// Simulate abrupt resource loss without the orderly Close checkpoint.
	a.owner.Close()
	a.root.Close()
	b, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s := b.Status()
	if len(s.UncertainCoverage) == 0 || s.UncertainCoverage[0].From.IsZero() {
		t.Fatalf("crash not accounted %+v", s)
	}
}

func TestUncertainPartialAppendQuarantinesCachedSegment(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(dir, func() bool { return true })
	defer a.Close()
	now := time.Now().UTC()
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: now})
	name := a.active["requests"]
	f, err := os.OpenFile(filepath.Join(dir, "history", name), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"schema_version":1,"request_id":"unfinished`)
	if _, err = f.Write(partial); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()
	// The append layer reports the interrupted write; cached offsets must no
	// longer be eligible for the next accepted append.
	a.mu.Lock()
	a.inFlight = name
	a.writeError()
	a.mu.Unlock()
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: now})
	p, err := a.Requests(Query{})
	if err != nil || len(p.Records) != 2 {
		t.Fatalf("recovery %+v %v", p, err)
	}
	if a.active["requests"] == name {
		t.Fatal("reused uncertain segment")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "history", name))
	if !strings.HasSuffix(string(raw), string(partial)) {
		t.Fatal("partial bytes modified")
	}
}
