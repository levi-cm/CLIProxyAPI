package accountpolicyledger

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedSegmentedArchive spreads 300 requests over several small segments, so the
// lookup root splits and the newest segment differs from sealed older ones.
func seedSegmentedArchive(t *testing.T) (string, *Archive, []string) {
	t.Helper()
	dir := t.TempDir()
	a, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	a.maxSegmentBytes = 8 * 1024
	ids := make([]string, 300)
	for i := range ids {
		ids[i] = fmt.Sprintf("tail-%04d", i)
		if err := a.append("requests", ids[i], Request{SchemaVersion: 1, RequestID: ids[i], Provider: "codex", RequestedAt: time.Date(2026, 11, 1, 0, 0, i, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := a.names("requests")
	if err != nil || len(names) < 3 {
		t.Fatalf("fixture needs several segments: %v %v", names, err)
	}
	if split, err := a.lookupSplit(lookupBase("requests", "")); err != nil || !split {
		t.Fatalf("fixture did not split the root leaf: %v %v", split, err)
	}
	return dir, a, ids
}

// lookupSentinel marks the current lookup tree; restoreLookup deletes the whole
// tree and therefore the sentinel, whereas an incremental repair keeps it.
func lookupSentinel(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "history", "lookup", "requests", "sentinel")
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func lookupRebuilt(sentinel string) bool {
	_, err := os.Stat(sentinel)
	return os.IsNotExist(err)
}

// assertLookupMatchesIndexes checks the lookup equals what a full rebuild
// produces: exactly the index entries, each assigned to its segment.
func assertLookupMatchesIndexes(t *testing.T, a *Archive) {
	t.Helper()
	names, err := a.names("requests")
	if err != nil {
		t.Fatal(err)
	}
	var expected lookupSignature
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
		t.Fatalf("lookup differs from a full rebuild: %v", err)
	}
}

// truncateLastRecords drops the newest n lines from a segment, as a copy taken
// while the writer appends sees the segment before its later records.
func truncateLastRecords(t *testing.T, dir, name string, n int) {
	t.Helper()
	p := filepath.Join(dir, "history", name)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	end := len(raw)
	for i := 0; i < n; i++ {
		end = bytes.LastIndexByte(raw[:end-1], '\n') + 1
	}
	if err := os.WriteFile(p, raw[:end], 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRepairsLookupNewerThanTailSegmentWithoutFullRebuild(t *testing.T) {
	dir, a, ids := seedSegmentedArchive(t)
	tail := a.active["requests"]
	lost := ids[len(ids)-2:]
	other := ""
	for _, id := range ids {
		base := lookupLeaf(t, a, "requests", id)
		if base != lookupLeaf(t, a, "requests", lost[0]) && base != lookupLeaf(t, a, "requests", lost[1]) {
			other = base
			break
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// The lookup names the two newest records, but the tail segment (and its
	// now stale index) does not contain them.
	truncateLastRecords(t, dir, tail, len(lost))
	sentinel := lookupSentinel(t, dir)
	before := leafInode(t, dir, other)

	b, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if lookupRebuilt(sentinel) {
		t.Fatal("full lookup rebuild ran for entries beyond the tail segment")
	}
	if after := leafInode(t, dir, other); after != before {
		t.Fatal("repair rewrote an unaffected leaf")
	}
	if b.lookupMemo != nil {
		t.Fatal("lookup memo left enabled after startup")
	}
	if !b.Status().Healthy {
		t.Fatal("history unhealthy after repair")
	}
	assertLookupMatchesIndexes(t, b)
	for _, id := range lost {
		if found, err := b.hasID("requests", id); err != nil || found {
			t.Fatalf("entry beyond the tail segment survived: %v %v", found, err)
		}
	}
	// The dropped records are no longer duplicates and archive again.
	for _, id := range lost {
		if err := b.append("requests", id, Request{SchemaVersion: 1, RequestID: id, Provider: "codex", RequestedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if got := countArchivedRequests(t, dir); got != len(ids) {
		t.Fatalf("archived %d requests, want %d", got, len(ids))
	}
}

func TestStartupTailPruneFallsBackToFullRebuild(t *testing.T) {
	cases := map[string]func(t *testing.T, a *Archive, ids []string) (id, segment string){
		// Sealed segments never gain records: an extra entry there is not a
		// copy artefact.
		"extra entry in sealed segment": func(t *testing.T, a *Archive, ids []string) (string, string) {
			names, _ := a.names("requests")
			return "ghost-sealed", names[0]
		},
		"extra entry in unknown segment": func(t *testing.T, a *Archive, ids []string) (string, string) {
			return "ghost-unknown", "requests-00000000000000000999.jsonl"
		},
		// A record of the oldest segment assigned to the tail is pruned, then
		// found missing: the wrong segment aborts the repair.
		"entry assigned to wrong segment": func(t *testing.T, a *Archive, ids []string) (string, string) {
			return ids[0], a.active["requests"]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir, a, ids := seedSegmentedArchive(t)
			id, segment := mutate(t, a, ids)
			base := lookupLeaf(t, a, "requests", id)
			bucket, err := a.lookupBucket(base)
			if err != nil {
				t.Fatal(err)
			}
			bucket[lookupDigest(id)] = segment
			if err := a.writeLookup(base+".json", bucket, true); err != nil {
				t.Fatal(err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			sentinel := lookupSentinel(t, dir)

			b, err := New(dir, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if !lookupRebuilt(sentinel) {
				t.Fatal("unrepairable lookup accepted without a full rebuild")
			}
			assertLookupMatchesIndexes(t, b)
			if found, err := b.hasID("requests", ids[len(ids)-1]); err != nil || !found {
				t.Fatalf("rebuild lost the newest record: %v %v", found, err)
			}
		})
	}
}
