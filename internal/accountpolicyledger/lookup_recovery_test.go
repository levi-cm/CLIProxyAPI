package accountpolicyledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestRecoveryBatchSplitsAndPreservesFirstSegment(t *testing.T) {
	a, err := New(t.TempDir(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.lookupDir("requests"); err != nil {
		t.Fatal(err)
	}
	// Shared prefixes force nested splits; a later segment repeats every ID.
	entries := map[string]string{}
	for i := 0; i < 4097; i++ {
		entries[fmt.Sprintf("000%061x", i)] = "requests-first.jsonl"
	}
	if err := a.mergeRecoveredLookup("requests", "", entries); err != nil {
		t.Fatal(err)
	}
	for digest := range entries {
		entries[digest] = "requests-later.jsonl"
	}
	entries[strings.Repeat("f", 64)] = "requests-later.jsonl"
	if err := a.mergeRecoveredLookup("requests", "", entries); err != nil {
		t.Fatal(err)
	}
	var expected lookupSignature
	for digest := range entries {
		segment := "requests-first.jsonl"
		if digest == strings.Repeat("f", 64) {
			segment = "requests-later.jsonl"
		}
		expected.add(digest, segment)
	}
	actual, err := a.lookupSignature("requests", "")
	if err != nil || actual != expected {
		t.Fatalf("rebuilt tree lost ownership: %+v %v", actual, err)
	}
	// Normal durable writes must still work against the rebuilt layout.
	if err := a.markID("requests", "new-request", "requests-later.jsonl"); err != nil {
		t.Fatal(err)
	}
	if found, err := a.hasID("requests", "new-request"); err != nil || !found {
		t.Fatalf("append after recovery: %v %v", found, err)
	}
}

func TestRecoveryBatchRejectsUnsafeMetadata(t *testing.T) {
	for _, suffix := range []string{".json", ".json.tmp", ".split", ".split.tmp"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			a, err := New(dir, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if err := a.lookupDir("requests"); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.WriteFile(outside, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, "history", "lookup", "requests", "root"+suffix)
			if err := os.Symlink(outside, name); err != nil {
				t.Fatal(err)
			}
			entries := map[string]string{lookupDigest("fixture"): "requests-first.jsonl"}
			if suffix == ".split.tmp" {
				for i := 0; i < lookupBucketEntries; i++ {
					entries[lookupDigest(fmt.Sprint(i))] = "requests-first.jsonl"
				}
			}
			if err := a.mergeRecoveredLookup("requests", "", entries); err == nil {
				t.Fatal("unsafe recovery metadata accepted")
			}
			if raw, err := os.ReadFile(outside); err != nil || string(raw) != "original" {
				t.Fatal("recovery touched symlink target")
			}
		})
	}
}

func TestLookupRecoveryPreservesDeduplicationAndRawHistory(t *testing.T) {
	for _, damage := range []string{"missing-tree", "missing-child", "corrupt-child", "empty-child", "both-derived-indexes"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			a, err := New(dir, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			a.maxSegmentBytes = 32768
			at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
			records := make([]usage.Record, 270)
			for i := range records {
				records[i] = usage.Record{RequestID: uuid.NewString(), RequestedAt: at, Provider: "codex", AuthID: "fixture"}
				a.HandleUsage(context.Background(), records[i])
			}
			observation := Observation{ID: "quota-fixture", CredentialID: "fixture", ObservedAt: at, DurationSeconds: 604800}
			if err := a.Observe(observation); err != nil {
				t.Fatal(err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			rawNames, _ := filepath.Glob(filepath.Join(dir, "history", "*.jsonl"))
			before := map[string][32]byte{}
			for _, name := range rawNames {
				raw, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				before[name] = sha256.Sum256(raw)
			}
			digest := sha256.Sum256([]byte(records[0].RequestID))
			child := filepath.Join(dir, "history", "lookup", "requests", hex.EncodeToString(digest[:])[:1]+".json")
			switch damage {
			case "missing-tree":
				err = os.RemoveAll(filepath.Join(dir, "history", "lookup"))
			case "missing-child":
				err = os.Remove(child)
			case "corrupt-child", "both-derived-indexes":
				err = os.WriteFile(child, []byte("{invalid"), 0600)
			case "empty-child":
				err = os.WriteFile(child, []byte("{}"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if damage == "both-derived-indexes" {
				for _, name := range rawNames {
					if err := os.Remove(name + ".index.json"); err != nil {
						t.Fatal(err)
					}
				}
			}
			a, err = New(dir, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			// Reopen the recovered tree before appending: startup must accept its
			// complete signature and deduplication must survive the second boot.
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			a, err = New(dir, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			for _, record := range records {
				a.HandleUsage(context.Background(), record)
			}
			if err := a.Observe(observation); err != nil {
				t.Fatal(err)
			}
			page, err := a.Requests(Query{Limit: 500})
			if err != nil || len(page.Records) != 270 {
				t.Fatalf("recovery duplicated/lost usage: %d %v", len(page.Records), err)
			}
			quota := 0
			if err := a.Allowance(Query{CredentialID: "fixture", From: at.Add(-time.Minute), To: at.Add(time.Minute)}, 604800, func(Observation) error { quota++; return nil }); err != nil || quota != 1 {
				t.Fatalf("quota duplicated: %d %v", quota, err)
			}
			if status := a.Status(); !status.Healthy || status.WriteErrors != 0 || status.Duplicates != 271 {
				t.Fatalf("recovery status: %+v", status)
			}
			for name, hash := range before {
				raw, err := os.ReadFile(name)
				if err != nil || sha256.Sum256(raw) != hash {
					t.Fatal("derived recovery modified raw history")
				}
			}
		})
	}
}
