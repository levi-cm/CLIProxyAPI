package accountpolicyledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyImportRestartableAndOriginalUntouched(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"schema_version":1,"request_id":"b31381aa-38a1-430c-8b1a-3de78de6965c","provider":"codex","credential_id":"fixture","model":"legacy-alias","requested_at":"2025-01-01T00:00:00Z","input_tokens":100,"total_tokens":100}` + "\n")
	path := filepath.Join(dir, "usage.2.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, _ := New(dir, func() bool { return true })
	defer a.Close()
	for i := 0; i < 2; i++ {
		if err := a.ImportLegacy(dir); err != nil {
			t.Fatal(err)
		}
	}
	p, err := a.Requests(Query{})
	if err != nil || len(p.Records) != 1 || !p.Records[0].Legacy || p.Records[0].CachedInputPercent != nil {
		t.Fatalf("%+v %v", p, err)
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != string(raw) {
		t.Fatal("source modified")
	}
}

func TestLegacyImportWhenEnabledAfterStartup(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"schema_version":1,"request_id":"b31381aa-38a1-430c-8b1a-3de78de6965c","provider":"codex","credential_id":"fixture","model":"legacy-alias","requested_at":"2025-01-01T00:00:00Z","input_tokens":100,"total_tokens":100}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	enabled := false
	a, _ := New(dir, func() bool { return enabled })
	defer a.Close()
	if err := a.ImportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	enabled = true
	p, err := a.Requests(Query{})
	if err != nil || len(p.Records) != 1 {
		t.Fatalf("late adoption %+v %v", p, err)
	}
}

// Read-only queries never initialize the archive or run a pending import.
func TestReadOnlyQueriesNeverPrepare(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"schema_version":1,"request_id":"b31381aa-38a1-430c-8b1a-3de78de6965c","provider":"codex","credential_id":"fixture","model":"legacy-alias","requested_at":"2025-01-01T00:00:00Z","input_tokens":100,"total_tokens":100}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	enabled := false
	a, _ := New(dir, func() bool { return enabled })
	defer a.Close()
	if err := a.ImportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	enabled = true // an import is now pending
	now := time.Now()
	if _, err := a.SummaryContext(context.Background(), Query{From: now.Add(-time.Hour), To: now, ReadOnly: true}, "day"); err == nil {
		t.Fatal("read-only summary ran on an unprepared archive")
	}
	if err := a.Allowance(Query{CredentialID: "fixture", From: now.Add(-time.Hour), To: now, ReadOnly: true}, 604800, func(Observation) error { return nil }); err == nil {
		t.Fatal("read-only allowance ran on an unprepared archive")
	}
	if s := a.Status(); s.Written != 0 {
		t.Fatalf("read-only queries wrote %d records", s.Written)
	}
	// A normal query prepares; read-only queries then work.
	if _, err := a.Requests(Query{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SummaryContext(context.Background(), Query{From: now.Add(-time.Hour), To: now, ReadOnly: true}, "day"); err != nil {
		t.Fatalf("read-only summary on a ready archive: %v", err)
	}
}
