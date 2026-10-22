package accountpolicyledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestArchiveRetentionRotationRestartAndStableCursor(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	a.maxSegmentBytes = 4096
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 800; i++ {
		a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "codex", AuthID: "fixture", Model: "execution", Alias: "alias", RequestedAt: start.AddDate(0, 0, i), Detail: usage.Detail{InputTokens: 100, CacheReadTokens: 8, TotalTokens: 100}})
	}
	if a.Status().WriteErrors != 0 {
		t.Fatalf("status %+v", a.Status())
	}
	page, err := a.Requests(Query{Limit: 100})
	if err != nil || len(page.Records) != 100 || page.NextCursor == "" {
		t.Fatalf("page %+v %v", page, err)
	}
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: start.AddDate(0, 0, 900)})
	count := len(page.Records)
	for page.NextCursor != "" {
		page, err = a.Requests(Query{Limit: 100, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Records)
	}
	if count != 800 {
		t.Fatalf("unstable pagination: %d", count)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "history", "requests-*.jsonl"))
	if len(files) < 2 {
		t.Fatal("rotation missing")
	}
	for _, f := range files {
		_ = os.Remove(f + ".index.json")
	}
	a, err = New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sums, err := a.Summary(Query{}, "year")
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, s := range sums {
		n += s.Attempts
	}
	if n != 801 {
		t.Fatalf("retained %d", n)
	}
}

func TestRecoveryPreservesCorruptCompleteRecordsAndTail(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(dir, func() bool { return true })
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: time.Now()})
	a.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "history", "requests-*.jsonl"))
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0600)
	fmt.Fprint(f, "{corrupt}\n{unfinished")
	f.Close()
	a, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p, err := a.Requests(Query{})
	if err != nil || len(p.Records) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	if a.Status().Healthy {
		t.Fatal("corruption must be visible")
	}
	data, _ := os.ReadFile(files[0])
	if string(data[len(data)-len("{unfinished"):]) != "{unfinished" {
		t.Fatal("tail must be preserved")
	}
	a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), RequestedAt: time.Now()})
	p, err = a.Requests(Query{})
	if err != nil || len(p.Records) != 2 {
		t.Fatalf("recovered writer: %+v %v", p, err)
	}
}

func TestUnsafeArchivePath(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "history")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, func() bool { return true }); err == nil {
		t.Fatal("accepted symlink")
	}
}
