// Package accountpolicybindings persists verified conversation ownership, not
// prompts, response bodies, credentials, or routing recommendations.
package accountpolicybindings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const maxFileBytes = 64 << 20
const maxRecords = 65536
const stateName = "conversation-bindings.json"

var ErrUnavailable = errors.New("conversation ownership state unavailable")
var ErrConflict = errors.New("conversation ownership changed")

type Key struct {
	CallerHash  string `json:"caller_hash"`
	Provider    string `json:"provider"`
	SessionHash string `json:"session_hash"`
	Model       string `json:"model"`
}

type Owner struct {
	CredentialID string `json:"credential_id"`
	AccountID    string `json:"account_id"`
	WorkspaceID  string `json:"workspace_id"`
}

type record struct {
	Key         Key       `json:"key"`
	Owner       Owner     `json:"owner"`
	CompletedAt time.Time `json:"completed_at"`
}

type diskState struct {
	Version int      `json:"version"`
	Records []record `json:"records"`
}

type Store struct {
	mu  sync.Mutex
	dir string
}

// New is lazy to preserve disabled-policy startup and avoids a second lifecycle
// lock owner. Every atomic update reloads under the cross-process file lock.
func New(dir string) *Store { return &Store{dir: dir} }

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func NewKey(caller, provider, session, model string) Key {
	if caller == "" || provider == "" || session == "" || model == "" {
		return Key{}
	}
	return Key{hash(caller), provider, hash(session), model}
}

func (k Key) valid() bool {
	_, callerErr := hex.DecodeString(k.CallerHash)
	_, sessionErr := hex.DecodeString(k.SessionHash)
	return callerErr == nil && sessionErr == nil && len(k.CallerHash) == 64 && len(k.SessionHash) == 64 && k.Provider != "" && len(k.Provider) <= 128 && k.Model != "" && len(k.Model) <= 1024
}

func (k Key) id() string { data, _ := json.Marshal(k); return hash(string(data)) }
func (o Owner) valid() bool {
	return o.CredentialID != "" && o.AccountID != "" && o.WorkspaceID != "" && len(o.CredentialID) <= 4096 && len(o.AccountID) <= 4096 && len(o.WorkspaceID) <= 4096
}

func (s *Store) open(create bool) (*os.Root, error) {
	if s.dir == "" {
		return nil, ErrUnavailable
	}
	abs, err := filepath.Abs(s.dir)
	if err != nil {
		return nil, ErrUnavailable
	}
	for path := abs; ; path = filepath.Dir(path) {
		info, errStat := os.Lstat(path)
		if errStat != nil && !os.IsNotExist(errStat) || errStat == nil && !info.IsDir() {
			return nil, ErrUnavailable
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	if create {
		if err := os.MkdirAll(abs, 0700); err != nil {
			return nil, ErrUnavailable
		}
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		if !create && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, ErrUnavailable
	}
	return root, nil
}

func load(root *os.Root) (map[string]record, error) {
	items := map[string]record{}
	if root == nil {
		return items, nil
	}
	f, err := root.OpenFile(stateName, os.O_RDONLY|noFollow, 0)
	if os.IsNotExist(err) {
		return items, nil
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxFileBytes {
		return nil, ErrUnavailable
	}
	var disk diskState
	decoder := json.NewDecoder(io.LimitReader(f, maxFileBytes+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&disk) != nil || disk.Version != 1 || len(disk.Records) > maxRecords {
		return nil, ErrUnavailable
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, ErrUnavailable
	}
	for _, item := range disk.Records {
		if !item.Key.valid() || !item.Owner.valid() || item.CompletedAt.IsZero() {
			return nil, ErrUnavailable
		}
		id := item.Key.id()
		if _, duplicate := items[id]; duplicate {
			return nil, ErrUnavailable
		}
		items[id] = item
	}
	return items, nil
}

func (s *Store) Lookup(key Key) (Owner, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !key.valid() {
		return Owner{}, false, ErrUnavailable
	}
	root, err := s.open(false)
	if err != nil {
		return Owner{}, false, err
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	items, err := load(root)
	item, found := items[key.id()]
	return item.Owner, found, err
}

// Put uses ownership CAS: an obsolete in-flight completion cannot replace a
// newer owner. Never evict established ownership just to admit a new session.
func (s *Store) Put(key Key, owner Owner, expected *Owner) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !key.valid() || !owner.valid() {
		return ErrUnavailable
	}
	root, err := s.open(true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	unlock, err := lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	items, err := load(root)
	if err != nil {
		return err
	}
	id := key.id()
	previous, found := items[id]
	if found && previous.Owner != owner && (expected == nil || *expected != previous.Owner) {
		return ErrConflict
	}
	if !found && expected != nil {
		return ErrConflict
	}
	if !found && len(items) >= maxRecords {
		return ErrUnavailable
	}
	items[id] = record{Key: key, Owner: owner, CompletedAt: time.Now().UTC()}
	keys := make([]string, 0, len(items))
	for id := range items {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	disk := diskState{Version: 1, Records: make([]record, 0, len(keys))}
	for _, id := range keys {
		disk.Records = append(disk.Records, items[id])
	}
	data, err := json.Marshal(disk)
	if err != nil || len(data) > maxFileBytes {
		return ErrUnavailable
	}
	// An unpredictable temporary name plus O_EXCL prevents symlink replacement.
	tmp := ".conversation-bindings-" + hash(time.Now().String())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0600)
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = root.Remove(tmp) }()
	_, errWrite := f.Write(data)
	errSync, errClose := f.Sync(), f.Close()
	if errors.Join(errWrite, errSync, errClose) != nil {
		return ErrUnavailable
	}
	if root.Rename(tmp, stateName) != nil {
		return ErrUnavailable
	}
	directory, err := root.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	errSync, errClose = directory.Sync(), directory.Close()
	if errors.Join(errSync, errClose) != nil {
		return ErrUnavailable
	}
	return nil
}
