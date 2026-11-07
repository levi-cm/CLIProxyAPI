package accountpolicyledger

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func legacyLines(from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, `{"request_id":"memo-%05d","provider":"codex","credential_id":"fixture","model":"m","requested_at":"2026-11-01T00:00:00Z","input_tokens":1,"total_tokens":1}`+"\n", i)
	}
	return b.String()
}

func countArchivedRequests(t *testing.T, dir string) int {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "history", "requests-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 1024*1024), 1024*1024)
		for s.Scan() {
			if len(s.Bytes()) > 0 {
				total++
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return total
}

// The import memo must never hide records written during the same import,
// including across lookup leaf splits, and must leave a lookup that matches the
// segment indexes exactly.
func TestLegacyImportMemoKeepsDeduplicationAcrossSplits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "usage.2.jsonl"), []byte(legacyLines(0, 300)), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ImportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	// The newer segment repeats every earlier record, repeats its own records
	// within the same file, and adds enough new ones to split lookup leaves.
	newer := legacyLines(0, 600) + legacyLines(350, 450)
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte(newer), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ImportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	if a.lookupMemo != nil {
		t.Fatal("lookup memo left enabled after import")
	}
	if got := countArchivedRequests(t, dir); got != 600 {
		t.Fatalf("archived %d requests, want 600 unique", got)
	}
	for _, id := range []string{"memo-00000", "memo-00299", "memo-00300", "memo-00599"} {
		if found, err := a.hasID("requests", id); err != nil || !found {
			t.Fatalf("hasID(%s) = %v, %v", id, found, err)
		}
	}
	var expected lookupSignature
	names, err := a.names("requests")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		idx, err := a.index(name)
		if err != nil {
			t.Fatal(err)
		}
		for id := range idx.IDs {
			expected.add(lookupDigest(id), name)
		}
	}
	actual, err := a.lookupSignature("requests", "")
	if err != nil || actual != expected {
		t.Fatalf("lookup diverged from segment indexes: %+v vs %+v, err=%v", actual, expected, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// A restart imports nothing new.
	b, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte(newer+legacyLines(599, 600)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	if got := countArchivedRequests(t, dir); got != 600 {
		t.Fatalf("restart import archived %d requests, want 600", got)
	}
}
