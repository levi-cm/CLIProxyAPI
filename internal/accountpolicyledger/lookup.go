package accountpolicyledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
)

const lookupBucketEntries = 256

// The derived UUID index is a disk radix tree with bounded leaf buckets. Splits
// commit children before their parent split marker; interruption leaves either
// the old leaf or the complete children authoritative. Raw segments rebuild it.
// Ordinary appends never load UUID indexes from all historical segments.
func (a *Archive) lookupDir(kind string) error {
	for _, name := range []string{"lookup", "lookup/" + kind} {
		info, err := a.root.Lstat(name)
		if os.IsNotExist(err) {
			if err = a.root.Mkdir(name, 0700); err != nil {
				return err
			}
			parent, err := a.root.Open(path.Dir(name))
			if err != nil {
				return err
			}
			if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe lookup directory")
		}
	}
	return nil
}
func lookupDigest(id string) string { h := sha256.Sum256([]byte(id)); return hex.EncodeToString(h[:]) }
func lookupBase(kind, prefix string) string {
	if prefix == "" {
		prefix = "root"
	}
	return "lookup/" + kind + "/" + prefix
}
func (a *Archive) lookupSplit(base string) (bool, error) {
	info, err := a.root.Lstat(base + ".split")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("unsafe lookup split")
	}
	return true, nil
}
func (a *Archive) lookupBucket(base string) (map[string]string, error) {
	info, err := a.root.Lstat(base + ".json")
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128*1024 {
		return nil, errors.New("unsafe lookup bucket")
	}
	f, err := a.root.Open(base + ".json")
	if err != nil {
		return nil, err
	}
	var bucket map[string]string
	err = json.NewDecoder(io.LimitReader(f, 128*1024)).Decode(&bucket)
	err = errors.Join(err, f.Close())
	if err != nil || bucket == nil || len(bucket) > lookupBucketEntries {
		return nil, errors.New("invalid lookup bucket")
	}
	return bucket, nil
}
func (a *Archive) saveLookup(name string, v any) error {
	for _, n := range []string{name, name + ".tmp"} {
		info, err := a.root.Lstat(n)
		if err == nil && !info.Mode().IsRegular() {
			return errors.New("unsafe lookup metadata")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := a.root.OpenFile(name+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(v)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = a.root.Rename(name+".tmp", name); err != nil {
		return err
	}
	d, err := a.root.Open(path.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
func (a *Archive) markID(kind, id, segment string) error {
	if err := a.lookupDir(kind); err != nil {
		return err
	}
	return a.putLookup(kind, "", lookupDigest(id), segment)
}
func (a *Archive) putLookup(kind, prefix, digest, segment string) error {
	if len(prefix) >= len(digest) {
		return errors.New("lookup hash collision")
	}
	base := lookupBase(kind, prefix)
	split, err := a.lookupSplit(base)
	if err != nil {
		return err
	}
	if split {
		return a.putLookup(kind, digest[:len(prefix)+1], digest, segment)
	}
	bucket, err := a.lookupBucket(base)
	if err != nil {
		return err
	}
	if existing, ok := bucket[digest]; ok {
		if existing == segment {
			return nil
		}
		return nil
	}
	if len(bucket) < lookupBucketEntries {
		bucket[digest] = segment
		return a.saveLookup(base+".json", bucket)
	}
	children := map[string]map[string]string{}
	for key, value := range bucket {
		child := key[:len(prefix)+1]
		if children[child] == nil {
			children[child] = map[string]string{}
		}
		children[child][key] = value
	}
	for child, items := range children {
		if err = a.saveLookup(lookupBase(kind, child)+".json", items); err != nil {
			return err
		}
	}
	if err = a.saveLookup(base+".split", map[string]int{"version": 1}); err != nil {
		return err
	}
	return a.putLookup(kind, digest[:len(prefix)+1], digest, segment)
}
func (a *Archive) hasID(kind, id string) (bool, error) {
	digest := lookupDigest(id)
	prefix := ""
	for len(prefix) < len(digest) {
		base := lookupBase(kind, prefix)
		split, err := a.lookupSplit(base)
		if err != nil {
			return false, err
		}
		if split {
			prefix = digest[:len(prefix)+1]
			continue
		}
		bucket, err := a.lookupBucket(base)
		if err != nil {
			return false, err
		}
		_, found := bucket[digest]
		return found, nil
	}
	return false, errors.New("invalid lookup depth")
}
