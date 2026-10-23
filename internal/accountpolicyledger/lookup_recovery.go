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
		for id := range idx.IDs {
			if err := a.markID(kind, id, name); err != nil {
				return err
			}
		}
	}
	return nil
}
