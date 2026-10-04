package accountpolicyusage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func openSink(t *testing.T, dir string, enabled func() bool) *Sink {
	t.Helper()
	sink, err := New(dir, enabled)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := sink.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	return sink
}

func readEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "usage") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		file, errOpen := os.Open(filepath.Join(dir, entry.Name()))
		if errOpen != nil {
			t.Fatal(errOpen)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var event map[string]any
			if errDecode := json.Unmarshal(scanner.Bytes(), &event); errDecode != nil {
				t.Error(errDecode)
			}
			events = append(events, event)
		}
		if errScan := scanner.Err(); errScan != nil {
			t.Error(errScan)
		}
		if errClose := file.Close(); errClose != nil {
			t.Error(errClose)
		}
	}
	return events
}

// Serializing the SDK record directly would leak each of these forbidden fields.
func TestSinkPersistsOnlyAllowlistedUsage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "policy")
	sink := openSink(t, dir, func() bool { return true })
	requestedAt := time.Date(2026, 10, 4, 10, 30, 0, 0, time.UTC)
	sink.HandleUsage(context.Background(), usage.Record{
		RequestID: uuid.NewString(), Provider: "codex", AuthID: "credential-a", Model: "upstream-model", Alias: "logical-model",
		RequestedAt: requestedAt, Latency: 2500 * time.Millisecond, TTFT: 125 * time.Millisecond,
		APIKey: "secret-api-key", AccessTokenSHA256: "secret-token-hash", Source: "secret-source",
		BaseURL: "https://private.example/secret-url", SessionID: "secret-session", ParentSessionID: "secret-parent",
		Fail: usage.Failure{StatusCode: 429, Body: "secret-prompt-body"}, Failed: true,
		ResponseHeaders: http.Header{"Authorization": {"secret-header"}},
		Detail:          usage.Detail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 4, TotalTokens: 120, CachedTokens: 35, CacheCreationTokens: 6},
	})
	events := readEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("want one durable completion, got %d", len(events))
	}
	event := events[0]
	allowed := map[string]bool{"schema_version": true, "request_id": true, "provider": true, "credential_id": true, "model": true,
		"requested_at": true, "latency_ms": true, "ttft_ms": true, "failed": true, "status_code": true,
		"input_tokens": true, "output_tokens": true, "reasoning_tokens": true, "total_tokens": true,
		"cache_read_tokens": true, "cache_write_tokens": true}
	for key := range event {
		if !allowed[key] {
			t.Errorf("unexpected stored field: %s", key)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-") || strings.Contains(string(data), "private.example") {
		t.Fatal("sensitive SDK fields were persisted")
	}
	for key, want := range map[string]any{"provider": "codex", "credential_id": "credential-a", "model": "logical-model",
		"requested_at": "2026-10-04T10:30:00Z", "latency_ms": float64(2500), "ttft_ms": float64(125),
		"cache_read_tokens": float64(35), "cache_write_tokens": float64(6), "total_tokens": float64(120), "status_code": float64(429)} {
		if event[key] != want {
			t.Errorf("%s = %v, want %v", key, event[key], want)
		}
	}
	for _, path := range []string{dir, filepath.Join(dir, "usage.jsonl")} {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatal(errStat)
		}
		wantMode := os.FileMode(0600)
		if path == dir {
			wantMode = 0700
		}
		if info.Mode().Perm() != wantMode {
			t.Errorf("permissions = %o, want %o", info.Mode().Perm(), wantMode)
		}
	}
}

func TestSinkUnavailableMetricsRemainNull(t *testing.T) {
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	sink.HandleUsage(context.Background(), usage.Record{RequestID: "secret-request-id", Provider: "codex"})
	event := readEvents(t, dir)[0]
	for _, key := range []string{"cache_read_tokens", "cache_write_tokens", "ttft_ms"} {
		value, exists := event[key]
		if !exists || value != nil {
			t.Errorf("%s must be explicitly unavailable, got %v", key, value)
		}
	}
	if _, err := uuid.Parse(event["request_id"].(string)); err != nil {
		t.Errorf("non-UUID request identifier was stored: %v", event["request_id"])
	}
}

func TestSinkDisabledCreatesNothingAndCanEnableLater(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	enabled := false
	sink := openSink(t, dir, func() bool { return enabled })
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("disabled sink touched state directory: %v", err)
	}
	enabled = true
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	enabled = false
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	if got := len(readEvents(t, dir)); got != 1 {
		t.Fatalf("want only the enabled completion, got %d", got)
	}
}

func TestSinkRestartPreservesCompletionsAndDeduplicatesRequest(t *testing.T) {
	dir := t.TempDir()
	record := usage.Record{RequestID: uuid.NewString(), Provider: "codex", Detail: usage.Detail{TotalTokens: 50}}
	first := openSink(t, dir, func() bool { return true })
	first.HandleUsage(context.Background(), record)
	first.HandleUsage(context.Background(), record)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := openSink(t, dir, func() bool { return true })
	second.HandleUsage(context.Background(), record)
	second.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "codex"})
	if got := len(readEvents(t, dir)); got != 2 {
		t.Fatalf("durable request IDs must survive restart, got %d records", got)
	}
}

func TestSinkConcurrentAppendsStayComplete(t *testing.T) {
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "codex"})
		}()
	}
	workers.Wait()
	events := readEvents(t, dir)
	if len(events) != 64 {
		t.Fatalf("lost or corrupted concurrent records: got %d", len(events))
	}
}

func TestSinkRotationBoundsStorageAndRetainsNewestEvent(t *testing.T) {
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	sink.maxFileBytes = 1024
	for i := 0; i < 30; i++ {
		sink.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "codex", Model: fmt.Sprintf("model-%d", i)})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("rotation should retain exactly three segments, got %d", len(entries))
	}
	for _, entry := range entries {
		info, errInfo := entry.Info()
		if errInfo != nil {
			t.Fatal(errInfo)
		}
		if info.Size() > 1024 {
			t.Errorf("segment %s exceeds bound: %d", entry.Name(), info.Size())
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("rotated segment permissions = %o", info.Mode().Perm())
		}
	}
	latest, errRead := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if errRead != nil || !strings.Contains(string(latest), "model-29") {
		t.Fatalf("newest completion lost: %v", errRead)
	}
}

func TestSinkRejectsSymlinkStateFiles(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "unchanged")
	if err := os.WriteFile(outside, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "usage.jsonl")); err != nil {
		t.Fatal(err)
	}
	sink := openSink(t, dir, func() bool { return true })
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "original" {
		t.Fatalf("state symlink modified outside target: %q, %v", data, err)
	}
	if sink.Summary().WriteErrors != 1 {
		t.Fatal("blocked write must be observable")
	}
}

func TestSinkCloseStopsFurtherWrites(t *testing.T) {
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	if got := len(readEvents(t, dir)); got != 1 {
		t.Fatalf("closed sink wrote additional records: %d", got)
	}
}

func TestSinkCanonicalZeroCacheReadDoesNotReuseLegacyTotal(t *testing.T) {
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex", Detail: usage.Detail{
		CachedTokens: 13, CacheCreationTokens: 13,
		TokenBreakdown: usage.NewSubsetTokenBreakdown(13, 0, 13, 0, 0, 13),
	}})
	event := readEvents(t, dir)[0]
	if event["cache_read_tokens"] != nil {
		t.Fatal("canonical zero reads cannot be replaced by a legacy combined cache total")
	}
	if event["cache_write_tokens"] != float64(13) {
		t.Fatal("measured creation tokens were lost")
	}
}

func TestSinkRestartRecoversOnlyUnfinishedFinalEvent(t *testing.T) {
	dir := t.TempDir()
	first := openSink(t, dir, func() bool { return true })
	first.HandleUsage(context.Background(), usage.Record{Provider: "codex", Model: "complete-before-crash"})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	file, errOpen := os.OpenFile(filepath.Join(dir, "usage.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	_, errWrite := file.WriteString(`{"request_id":"unfinished`)
	errClose := file.Close()
	if errWrite != nil || errClose != nil {
		t.Fatalf("prepare interrupted append: %v, %v", errWrite, errClose)
	}
	second := openSink(t, dir, func() bool { return true })
	second.HandleUsage(context.Background(), usage.Record{Provider: "codex", Model: "complete-after-restart"})
	events := readEvents(t, dir)
	if len(events) != 2 || events[0]["model"] != "complete-before-crash" || events[1]["model"] != "complete-after-restart" {
		t.Fatalf("unfinished append damaged complete durable events: %v", events)
	}
}

func TestSinkRejectsSymlinkInDirectoryPath(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(parent, "external")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(parent, "external", "policy"), func() bool { return true }); err == nil {
		t.Fatal("symlink ancestor must not direct usage writes outside requested directory tree")
	}
}

func TestSinkDisabledDefersUnsafeDirectoryCheckUntilEnabled(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(parent, "external")); err != nil {
		t.Fatal(err)
	}
	enabled := false
	sink := openSink(t, filepath.Join(parent, "external", "policy"), func() bool { return enabled })
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	if _, err := os.Stat(filepath.Join(outside, "policy")); !os.IsNotExist(err) {
		t.Fatalf("disabled sink touched unsafe state path: %v", err)
	}
	enabled = true
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex"})
	if _, err := os.Stat(filepath.Join(outside, "policy")); !os.IsNotExist(err) {
		t.Fatalf("enabled sink wrote through directory symlink: %v", err)
	}
	if got := sink.Summary().WriteErrors; got != 1 {
		t.Fatalf("deferred unsafe path must report one blocked write, got %d", got)
	}
}

func TestSinkRejectsOversizedEventsWithoutGrowingStorage(t *testing.T) {
	dir := t.TempDir()
	sink := openSink(t, dir, func() bool { return true })
	sink.maxFileBytes = 1024
	sink.HandleUsage(context.Background(), usage.Record{Provider: "codex", Model: strings.Repeat("x", 2048)})
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("oversized record grew storage: %v, %v", entries, err)
	}
	if sink.Summary().WriteErrors != 1 {
		t.Fatal("oversized dropped record was not observable")
	}
}
