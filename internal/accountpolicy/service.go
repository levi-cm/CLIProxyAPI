package accountpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

type storedState struct {
	Version    int                 `json:"version"`
	Revision   uint64              `json:"revision"`
	Settings   Settings            `json:"settings"`
	Snapshots  map[string]Snapshot `json:"snapshots"`
	Operations []Operation         `json:"operations"`
	Schedules  []Schedule          `json:"schedules"`
	Decisions  []Decision          `json:"decisions"`
}
type discoveryState struct {
	Next     time.Time
	Failures int
}
type Service struct {
	mu        sync.RWMutex
	state     storedState
	opts      Options
	locks     map[string]chan struct{}
	discovery map[string]discoveryState
	semaphore chan struct{}
	stateDir  string
	replica   bool
}

func clone[T any](v T) T {
	data, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(data, &out)
	return out
}

func NewService(opts Options) (*Service, error) {
	if err := ValidateSettings(opts.Settings); err != nil {
		return nil, err
	}
	if opts.Provider == nil || opts.Accounts == nil {
		return nil, policyError("invalid_settings", "provider and accounts are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Settings.Enabled && opts.Settings.StateDir == "" {
		return nil, policyError("invalid_settings", "enabled account policy requires durable state directory")
	}
	s := &Service{opts: opts, stateDir: opts.Settings.StateDir, locks: map[string]chan struct{}{}, discovery: map[string]discoveryState{}, semaphore: make(chan struct{}, 2), state: storedState{Version: 1, Settings: clone(opts.Settings), Snapshots: map[string]Snapshot{}, Operations: []Operation{}, Schedules: []Schedule{}, Decisions: []Decision{}}}
	seen := map[string]bool{}
	for _, id := range opts.Accounts() {
		if id.CredentialID == "" {
			return nil, policyError("invalid_settings", "empty account credential mapping")
		}
		logical := id.Provider + "\x00" + id.AccountID + "\x00" + id.WorkspaceID
		if id.AccountID != "" && seen[logical] {
			return nil, policyError("conflict", "duplicate logical account ownership")
		}
		seen[logical] = true
	}
	if s.stateDir != "" {
		if err := os.MkdirAll(s.stateDir, 0700); err != nil {
			return nil, policyError("persistence", "cannot create policy state directory")
		}
		data, err := os.ReadFile(filepath.Join(s.stateDir, "state.json"))
		if err == nil {
			if json.Unmarshal(data, &s.state) != nil || s.state.Version != 1 || ValidateSettings(s.state.Settings) != nil {
				return nil, policyError("persistence", "invalid policy journal")
			}
			if s.state.Snapshots == nil {
				s.state.Snapshots = map[string]Snapshot{}
			}
			s.state.Settings.StateDir = s.stateDir
			// A replica configured read-only cannot acquire authority from persisted settings.
			if opts.Settings.ReadOnly {
				s.state.Settings.ReadOnly = true
			}
			if !opts.Settings.Enabled {
				s.state.Settings.Enabled = false
				s.state.Settings.Automation = "off"
			}
			if opts.Settings.Automation == "off" || opts.Settings.Automation == "notify" {
				s.state.Settings.Automation = opts.Settings.Automation
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, policyError("persistence", "cannot read policy journal")
		}
	}
	return s, nil
}

func (s *Service) now() time.Time { return s.opts.Now().UTC() }
func (s *Service) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.state.Settings)
}
func (s *Service) Snapshot(id string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.state.Snapshots[id]
	return clone(v), ok
}
func (s *Service) Accounts() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Snapshot{}
	for _, v := range s.state.Snapshots {
		out = append(out, clone(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.CredentialID < out[j].Identity.CredentialID })
	return out
}
func (s *Service) Decisions() []Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.state.Decisions)
}
func (s *Service) Operations() []Operation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.state.Operations)
}
func (s *Service) Schedules() []Schedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.state.Schedules)
}

// persistLocked makes one atomic replacement of the complete policy state. Failed writes roll back memory.
func (s *Service) persistLocked(before storedState) error {
	if s.replica {
		return nil
	}
	if s.stateDir == "" {
		return nil
	}
	unlock, err := lockNamed(s.stateDir, "state.lock")
	if err != nil {
		s.state = before
		return err
	}
	defer unlock()
	if diskData, errRead := os.ReadFile(filepath.Join(s.stateDir, "state.json")); errRead == nil {
		var disk storedState
		if json.Unmarshal(diskData, &disk) != nil || disk.Revision != before.Revision {
			s.state = before
			return policyError("conflict", "policy state changed in another process")
		}
	} else if !errors.Is(errRead, os.ErrNotExist) {
		s.state = before
		return policyError("persistence", "cannot read policy journal revision")
	}
	s.state.Revision = before.Revision + 1
	data, err := json.Marshal(s.state)
	if err != nil {
		s.state = before
		return policyError("persistence", "cannot encode policy journal")
	}
	f, err := os.CreateTemp(s.stateDir, ".state-*")
	if err != nil {
		s.state = before
		return policyError("persistence", "cannot prepare policy journal")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	errClose := f.Close()
	if err == nil {
		err = errClose
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(s.stateDir, "state.json"))
	}
	if err != nil {
		s.state = before
		return policyError("persistence", "cannot commit policy journal")
	}
	dir, errOpen := os.Open(s.stateDir)
	if errOpen == nil {
		err = dir.Sync()
		errClose = dir.Close()
		if err == nil {
			err = errClose
		}
	} else {
		err = errOpen
	}
	if err != nil {
		return policyError("persistence", "cannot synchronize policy journal")
	}
	return nil
}
func (s *Service) MergeSettings(update func(Settings) (Settings, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := update(clone(s.state.Settings))
	if err != nil {
		return err
	}
	if err = ValidateSettings(next); err != nil {
		return err
	}
	if next.StateDir != s.stateDir {
		return policyError("invalid_settings", "state directory cannot change while running")
	}
	if s.opts.Settings.ReadOnly && !next.ReadOnly {
		return policyError("read_only", "replica configuration requires read-only mode")
	}
	before := clone(s.state)
	s.state.Settings = clone(next)
	return s.persistLocked(before)
}
func (s *Service) UpdateSettings(settings Settings) error {
	return s.MergeSettings(func(Settings) (Settings, error) { return settings, nil })
}
func (s *Service) RecordDecision(d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.At.IsZero() {
		d.At = s.now()
	}
	s.state.Decisions = append(s.state.Decisions, d)
	if len(s.state.Decisions) > 200 {
		s.state.Decisions = s.state.Decisions[len(s.state.Decisions)-200:]
	}
}
func (s *Service) identity(id string) (Identity, error) {
	for _, v := range s.opts.Accounts() {
		if v.CredentialID == id {
			return v, nil
		}
	}
	return Identity{}, policyError("unknown_account", "account credential mapping not found")
}
func sameIdentity(a, b Identity) bool {
	return a.CredentialID == b.CredentialID && a.Provider == b.Provider && (a.AccountID == "" || a.AccountID == b.AccountID) && (a.WorkspaceID == "" || a.WorkspaceID == b.WorkspaceID) && a.Generation == b.Generation
}
func (s *Service) accountLock(ctx context.Context, id Identity) (func(), error) {
	key := id.Provider + "\x00" + id.AccountID + "\x00" + id.WorkspaceID
	if id.AccountID == "" {
		key = id.CredentialID
	}
	s.mu.Lock()
	ch := s.locks[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.locks[key] = ch
	}
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) Refresh(ctx context.Context, id string) error {
	if id != "" {
		identity, err := s.identity(id)
		if err != nil {
			return err
		}
		unlock, err := s.accountLock(ctx, identity)
		if err != nil {
			return err
		}
		defer unlock()
		_, err = s.discoverLocked(ctx, identity)
		return err
	}
	ids := s.opts.Accounts()
	jobs := make(chan Identity)
	errs := make(chan error, len(ids))
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for identity := range jobs {
				if err := s.Refresh(ctx, identity.CredentialID); err != nil {
					errs <- err
				}
			}
		}()
	}
	for _, identity := range ids {
		select {
		case jobs <- identity:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}

func (s *Service) discoverLocked(ctx context.Context, id Identity) (Snapshot, error) {
	select {
	case s.semaphore <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	next, errRead := s.opts.Provider.Discover(ctx, id)
	next = clone(next)
	<-s.semaphore
	now := s.now()
	s.mu.Lock()
	old := s.state.Snapshots[id.CredentialID]
	before := clone(s.state)
	settings := s.state.Settings
	tracking := s.discovery[id.CredentialID]
	if errRead == nil && (!sameIdentity(id, next.Identity) || next.ObservedAt.After(now) || (!old.ObservedAt.IsZero() && next.ObservedAt.Before(old.ObservedAt))) {
		errRead = policyError("identity_mismatch", "provider evidence identity or observation order mismatch")
		next.WritesDisabled = true
	}
	if errRead != nil {
		old.Identity = id
		old.LastError = "provider discovery failed"
		old.WritesDisabled = old.WritesDisabled || next.WritesDisabled
		tracking.Failures++
		backoff := time.Duration(1<<min(tracking.Failures, 6)) * time.Second
		var delayed interface{ RetryDelay() time.Duration }
		if errors.As(errRead, &delayed) && delayed.RetryDelay() > backoff {
			backoff = delayed.RetryDelay()
		}
		tracking.Next = now.Add(backoff)
		s.discovery[id.CredentialID] = tracking
		s.state.Snapshots[id.CredentialID] = old
		errSave := s.persistLocked(before)
		s.mu.Unlock()
		if errSave != nil {
			return old, errSave
		}
		return old, policyError("provider_error", "provider discovery failed")
	}
	providerIdentity := next.Identity
	next.Identity = id
	if next.Identity.AccountID == "" {
		next.Identity.AccountID = providerIdentity.AccountID
	}
	if next.Identity.WorkspaceID == "" {
		next.Identity.WorkspaceID = providerIdentity.WorkspaceID
	}
	next.Version = old.Version + 1
	next.ObservedAt = next.ObservedAt.UTC()
	next.InventoryObservedAt = next.InventoryObservedAt.UTC()
	s.state.Snapshots[id.CredentialID] = clone(next)
	interval := 5 * time.Minute
	if next.ActiveRequests > 0 || next.ActiveBindings > 0 {
		interval = time.Minute
	}
	if s.opts.IsIdle != nil && !s.opts.IsIdle(id.CredentialID) {
		interval = time.Minute
	}
	for _, c := range next.Credits {
		if c.ExpiresAt != nil && c.ExpiresAt.After(now) && c.ExpiresAt.Sub(now) <= time.Hour {
			interval = time.Minute
		}
	}
	// Stable per-account jitter avoids synchronized provider polling without delaying expiry checks.
	var hash uint32
	for _, r := range id.CredentialID {
		hash = hash*31 + uint32(r)
	}
	tracking = discoveryState{Next: now.Add(interval + time.Duration(hash%6)*time.Second)}
	s.discovery[id.CredentialID] = tracking
	errSave := s.persistLocked(before)
	s.mu.Unlock()
	if errSave != nil {
		return next, errSave
	}
	if settings.Enabled && s.opts.Recover != nil && recovered(old, next) {
		if current, errCurrent := s.identity(id.CredentialID); errCurrent == nil && sameIdentity(id, current) {
			if errRecover := s.opts.Recover(ctx, old, next); errRecover != nil {
				return next, policyError("recovery_failed", "local cooldown reconciliation failed")
			}
		}
	}
	return next, nil
}

func recovered(before, after Snapshot) bool {
	if before.Identity.CredentialID == "" || !sameIdentity(before.Identity, after.Identity) || after.ObservedAt.Before(before.ObservedAt) {
		return false
	}
	for _, old := range before.Buckets {
		for _, next := range after.Buckets {
			if old.Scope != next.Scope || old.Model != next.Model || old.DurationSeconds != next.DurationSeconds || next.ObservedAt.Before(old.ObservedAt) {
				continue
			}
			if next.Allowed != nil && !*next.Allowed || next.UsedPercent >= 100 {
				continue
			}
			if (old.UsedPercent >= 100 || old.Allowed != nil && !*old.Allowed) || (next.ResetAt.After(old.ResetAt) && next.UsedPercent < old.UsedPercent) {
				return true
			}
		}
	}
	return false
}

func (s *Service) Schedule(id, creditID string, at time.Time) (Schedule, error) {
	snapshot, ok := s.Snapshot(id)
	if !ok {
		return Schedule{}, policyError("unknown_account", "account snapshot not found")
	}
	credit, ok := findCredit(snapshot, creditID)
	if !ok {
		return Schedule{}, policyError("unknown_credit", "selected credit not found")
	}
	now := s.now()
	if !at.After(now) || !usableCredit(credit, s.Settings(), now) || credit.ExpiresAt != nil && !at.Before(*credit.ExpiresAt) {
		return Schedule{}, policyError("invalid_schedule", "schedule must be before selected credit expiry and after current time")
	}
	sch := Schedule{ID: uuid.NewString(), CredentialID: id, CreditID: creditID, At: at.UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := clone(s.state)
	for _, old := range s.state.Schedules {
		if old.CredentialID == id && old.CreditID == creditID {
			return Schedule{}, policyError("conflict", "credit already has a schedule")
		}
	}
	s.state.Schedules = append(s.state.Schedules, sch)
	return sch, s.persistLocked(before)
}
func (s *Service) CancelSchedule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := clone(s.state)
	for i, sch := range s.state.Schedules {
		if sch.ID == id {
			s.state.Schedules = append(s.state.Schedules[:i], s.state.Schedules[i+1:]...)
			return s.persistLocked(before)
		}
	}
	return policyError("unknown_schedule", "schedule not found")
}
func findCredit(snapshot Snapshot, id string) (Credit, bool) {
	for _, credit := range snapshot.Credits {
		if credit.ID == id {
			return credit, true
		}
	}
	return Credit{}, false
}

func (s *Service) Run(ctx context.Context) {
	if s.stateDir != "" {
		unlock, err := lockNamed(s.stateDir, "owner.lock")
		if err != nil {
			s.mu.Lock()
			s.replica = true
			s.state.Settings.ReadOnly = true
			s.mu.Unlock()
		} else {
			defer unlock()
		}
	}
	if s.Settings().Enabled {
		_ = s.Refresh(ctx, "")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Tick(ctx)
		}
	}
}
