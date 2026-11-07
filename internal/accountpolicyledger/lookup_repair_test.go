package accountpolicyledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lookupLeaf returns the leaf base that holds (or would hold) id.
func lookupLeaf(t *testing.T, a *Archive, kind, id string) string {
	t.Helper()
	digest := lookupDigest(id)
	prefix := ""
	for {
		base := lookupBase(kind, prefix)
		split, err := a.lookupSplit(base)
		if err != nil {
			t.Fatal(err)
		}
		if !split {
			return base
		}
		prefix = digest[:len(prefix)+1]
	}
}

func leafInode(t *testing.T, dir, base string) uint64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, "history", base+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// seedSplitArchive writes enough requests to split the root lookup leaf.
func seedSplitArchive(t *testing.T) (string, *Archive, []string) {
	t.Helper()
	dir := t.TempDir()
	a, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 300)
	for i := range ids {
		ids[i] = fmt.Sprintf("repair-%04d", i)
		if err := a.append("requests", ids[i], Request{SchemaVersion: 1, RequestID: ids[i], Provider: "codex", RequestedAt: time.Date(2026, 11, 1, 0, 0, i, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	if split, err := a.lookupSplit(lookupBase("requests", "")); err != nil || !split {
		t.Fatalf("fixture did not split the root leaf: %v %v", split, err)
	}
	return dir, a, ids
}

func TestStartupRepairsMissingLookupEntriesWithoutFullRebuild(t *testing.T) {
	dir, a, ids := seedSplitArchive(t)
	// Simulate an abrupt stop after the segment append but before the lookup
	// write: the newest record has no lookup entry.
	lost := ids[len(ids)-1]
	lostBase := lookupLeaf(t, a, "requests", lost)
	bucket, err := a.lookupBucket(lostBase)
	if err != nil {
		t.Fatal(err)
	}
	delete(bucket, lookupDigest(lost))
	if err := a.writeLookup(lostBase+".json", bucket, true); err != nil {
		t.Fatal(err)
	}
	other := ""
	for _, id := range ids {
		if base := lookupLeaf(t, a, "requests", id); base != lostBase {
			other = base
			break
		}
	}
	if other == "" {
		t.Fatal("no second leaf")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	before := leafInode(t, dir, other)

	b, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if found, err := b.hasID("requests", lost); err != nil || !found {
		t.Fatalf("missing entry not repaired: %v %v", found, err)
	}
	if after := leafInode(t, dir, other); after != before {
		t.Fatal("repair rewrote an unaffected leaf; a full rebuild ran")
	}
	if b.lookupMemo != nil {
		t.Fatal("lookup memo left enabled after startup")
	}
	// A later append of the same ID is still a duplicate.
	if err := b.append("requests", lost, Request{SchemaVersion: 1, RequestID: lost, Provider: "codex", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := countArchivedRequests(t, dir); got != len(ids) {
		t.Fatalf("archived %d requests, want %d", got, len(ids))
	}
}

func TestStartupFallsBackToFullRebuildForStaleLookupEntries(t *testing.T) {
	dir, a, ids := seedSplitArchive(t)
	// An entry for a record that no segment contains cannot be repaired by
	// adding entries; the full rebuild must remove it.
	ghost := "ghost-request"
	ghostBase := lookupLeaf(t, a, "requests", ghost)
	bucket, err := a.lookupBucket(ghostBase)
	if err != nil {
		t.Fatal(err)
	}
	segment := ""
	for _, v := range bucket {
		segment = v
		break
	}
	if !strings.HasSuffix(segment, ".jsonl") {
		t.Fatalf("unexpected leaf contents %v", bucket)
	}
	bucket[lookupDigest(ghost)] = segment
	if err := a.writeLookup(ghostBase+".json", bucket, true); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := New(dir, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if found, err := b.hasID("requests", ghost); err != nil || found {
		t.Fatalf("stale entry survived startup: %v %v", found, err)
	}
	for _, id := range []string{ids[0], ids[len(ids)-1]} {
		if found, err := b.hasID("requests", id); err != nil || !found {
			t.Fatalf("rebuild lost %s: %v %v", id, found, err)
		}
	}
}
