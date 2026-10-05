package accountpolicybindings

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStoreRestartIsolationAndCompareAndSwap(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	key := NewKey("private-caller", "codex", "codex:thread", "gpt-5")
	a := Owner{CredentialID: "a", AccountID: "account-a", WorkspaceID: "workspace-a"}
	b := Owner{CredentialID: "b", AccountID: "account-b", WorkspaceID: "workspace-b"}
	if err := s.Put(key, a, nil); err != nil {
		t.Fatal(err)
	}
	got, found, err := New(dir).Lookup(key)
	if err != nil || !found || got != a {
		t.Fatalf("restored=%+v found=%v error=%v", got, found, err)
	}
	for _, other := range []Key{NewKey("other-caller", "codex", "codex:thread", "gpt-5"), NewKey("private-caller", "codex", "other-thread", "gpt-5"), NewKey("private-caller", "codex", "codex:thread", "other-model"), NewKey("private-caller", "other-provider", "codex:thread", "gpt-5")} {
		if _, found, err := s.Lookup(other); err != nil || found {
			t.Fatalf("cross-scope match=%v error=%v", found, err)
		}
	}
	if err := s.Put(key, b, &a); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(key, a, &a); err == nil {
		t.Fatal("late completion overwrote new owner")
	}
	got, _, _ = New(dir).Lookup(key)
	if got != b {
		t.Fatalf("owner=%+v want B", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "conversation-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-caller") || strings.Contains(string(data), "codex:thread") {
		t.Fatal("raw caller/session persisted")
	}
	info, err := os.Stat(filepath.Join(dir, "conversation-bindings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private mode=%v error=%v", info, err)
	}
}

func TestStoreConcurrentInstancesRetainEveryCompletedOwner(t *testing.T) {
	dir := t.TempDir()
	stores := []*Store{New(dir), New(dir)}
	owner := Owner{CredentialID: "a", AccountID: "account-a", WorkspaceID: "workspace-a"}
	var wg sync.WaitGroup
	errors := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errors <- stores[i%2].Put(NewKey("caller", "codex", fmt.Sprintf("thread-%d", i), "gpt-5"), owner, nil)
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 16; i++ {
		got, found, err := New(dir).Lookup(NewKey("caller", "codex", fmt.Sprintf("thread-%d", i), "gpt-5"))
		if err != nil || !found || got != owner {
			t.Fatalf("lost completed thread %d: %+v %v %v", i, got, found, err)
		}
	}
}

func TestStoreRejectsCorruptUnsafeAndOversizedState(t *testing.T) {
	for _, kind := range []string{"malformed", "version", "symlink", "public_mode", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "conversation-bindings.json")
			data := []byte(`{"version":2,"records":[]}`)
			if kind == "malformed" {
				data = []byte(`{`)
			}
			if kind == "oversized" {
				data = make([]byte, maxFileBytes+1)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "public_mode" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Rename(path, filepath.Join(dir, "other.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other.json", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := New(dir).Lookup(NewKey("caller", "codex", "thread", "model")); err == nil {
				t.Fatal("unsafe state accepted")
			}
		})
	}
}
