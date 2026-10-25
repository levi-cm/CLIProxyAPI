package accountpolicyledger

import (
	"crypto/sha256"
	"errors"
	"strings"
)

// A commutative checksum lets startup compare complete indexes while retaining
// only one segment index and one bounded lookup leaf, rather than every UUID.
type lookupSignature struct {
	Count uint64
	Hash  [sha256.Size]byte
}

func (s *lookupSignature) add(digest, segment string) {
	h := sha256.Sum256([]byte(digest + "\x00" + segment))
	s.Count++
	for i := range h {
		s.Hash[i] ^= h[i]
	}
}

func (s *lookupSignature) combine(other lookupSignature) {
	s.Count += other.Count
	for i := range s.Hash {
		s.Hash[i] ^= other.Hash[i]
	}
}

func (a *Archive) lookupSignature(kind, prefix string) (lookupSignature, error) {
	var result lookupSignature
	base := lookupBase(kind, prefix)
	split, err := a.lookupSplit(base)
	if err != nil {
		return result, err
	}
	if split {
		if len(prefix) >= sha256.Size*2-1 {
			return result, errors.New("invalid lookup depth")
		}
		for _, digit := range "0123456789abcdef" {
			child, err := a.lookupSignature(kind, prefix+string(digit))
			if err != nil {
				return result, err
			}
			result.combine(child)
		}
		return result, nil
	}
	bucket, err := a.lookupBucket(base)
	if err != nil {
		return result, err
	}
	for digest, segment := range bucket {
		if !strings.HasPrefix(digest, prefix) {
			return result, errors.New("misplaced lookup entry")
		}
		result.add(digest, segment)
	}
	return result, nil
}

func (a *Archive) restoreLookup(kind string, names []string) error {
	// Validate the namespace before deleting derived metadata. RemoveAll stays
	// confined to the archive root and never alters authoritative raw segments.
	if err := a.lookupDir(kind); err != nil {
		return err
	}
	if err := a.root.RemoveAll("lookup/" + kind); err != nil {
		return err
	}
	if err := a.lookupDir(kind); err != nil {
		return err
	}
	for _, name := range names {
		idx, err := a.index(name)
		if err != nil {
			return err
		}
		entries := make(map[string]string, len(idx.IDs))
		for id := range idx.IDs {
			entries[lookupDigest(id)] = name
		}
		if err := a.mergeRecoveredLookup(kind, "", entries); err != nil {
			return err
		}
	}
	return nil
}

// Recovery batches one segment at a time, keeping memory bounded by the segment
// size instead of total history. Each affected leaf is written once per batch.
// Only this disposable rebuild skips individual fsyncs: raw segments and normal
// appends retain their durability. Startup checks the entire lookup against the
// segment indexes before using it, so interrupted rebuilds are rebuilt again.
func (a *Archive) mergeRecoveredLookup(kind, prefix string, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	base := lookupBase(kind, prefix)
	split, err := a.lookupSplit(base)
	if err != nil {
		return err
	}
	if !split {
		bucket, err := a.lookupBucket(base)
		if err != nil {
			return err
		}
		for digest, segment := range entries {
			if _, exists := bucket[digest]; !exists {
				bucket[digest] = segment
			}
		}
		if len(bucket) <= lookupBucketEntries {
			return a.writeLookup(base+".json", bucket, false)
		}
		entries = bucket
	}
	if len(prefix) >= sha256.Size*2-1 {
		return errors.New("lookup hash collision")
	}
	children := map[string]map[string]string{}
	for digest, segment := range entries {
		child := digest[:len(prefix)+1]
		if children[child] == nil {
			children[child] = map[string]string{}
		}
		children[child][digest] = segment
	}
	for child, items := range children {
		if err := a.mergeRecoveredLookup(kind, child, items); err != nil {
			return err
		}
	}
	if !split {
		return a.writeLookup(base+".split", map[string]int{"version": 1}, false)
	}
	return nil
}
