package claudereset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// JournalFile is the journal's name inside the account-policy state directory.
const JournalFile = "claude-resets.json"

// Operation states.
const (
	OpPending = "pending"
	OpDone    = "done"
	OpFailed  = "failed"
)

// Result labels for outcomes the journal settles without a provider result.
const (
	ResultReconciled      = "reconciled_reset"
	ResultNotConsumed     = "not_consumed"
	ResultUnresolved      = "unresolved"
	ResultAccountGone     = "account_removed"
	ResultIdentity        = "identity_mismatch"
	ResultIdentityChanged = "identity_changed"
	// ResultNotSent marks a journaled first attempt abandoned before sending.
	ResultNotSent      = "not_sent"
	maxJournalEntries  = 200
	journalFileVersion = 1
)

// UnresolvedAfter settles a claim whose outcome still cannot be determined.
// It ends the account's quarantine; the grant inventory read before any later
// claim shows whether the reset was spent.
const UnresolvedAfter = time.Hour

// RecoveryWindow is the minimum time a spent reset's local recovery is
// retried; it is retried at least until the cleared windows would have renewed
// naturally, after which their cooldowns have expired anyway.
const RecoveryWindow = time.Hour

// RecoveryUntil is when an unfinished local recovery of op becomes obsolete.
func RecoveryUntil(op Operation) time.Time {
	until := op.UpdatedAt.Add(RecoveryWindow)
	for _, b := range []*WindowBaseline{op.WeeklyBefore, op.FableBefore} {
		if b != nil && b.ResetAt.After(until) {
			until = b.ResetAt
		}
	}
	return until
}

// WindowBaseline is one cleared window's usage and renewal at claim time.
type WindowBaseline struct {
	Used    float64   `json:"used"`
	ResetAt time.Time `json:"reset_at,omitzero"`
}

// FailureBackoff pauses claims on an account after a failed claim.
const FailureBackoff = time.Hour

// ClaimRetryWindow is how long an unresolved claim is retried with its
// original request ID. Afterwards it is settled from the grant inventory.
const ClaimRetryWindow = 9 * time.Minute

// Operation is one journaled claim. It is written before the claim is sent, so
// a crash never loses a request ID that may have spent a grant.
type Operation struct {
	ID               string    `json:"id"`
	RequestID        string    `json:"request_id,omitempty"`
	CredentialID     string    `json:"credential_id"`
	AccountUUID      string    `json:"account_uuid,omitempty"`
	OrganizationUUID string    `json:"organization_uuid,omitempty"`
	GrantID          string    `json:"grant_id"`
	GrantLabel       string    `json:"grant_label,omitempty"`
	Class            Class     `json:"class"`
	ClearsWeekly     bool      `json:"clears_weekly,omitempty"`
	ClearsFable      bool      `json:"clears_fable,omitempty"`
	Trigger          string    `json:"trigger"`
	Reason           string    `json:"reason,omitempty"`
	ResetsLeftBefore int       `json:"resets_left_before"`
	GrantEndsAt      time.Time `json:"grant_ends_at,omitzero"`
	// WeeklyBefore and FableBefore are the cleared windows at claim time, so a
	// vanished grant can be settled from each window's own usage evidence, and
	// recovery clears only cooldowns caused by those windows.
	WeeklyBefore *WindowBaseline `json:"weekly_before,omitempty"`
	FableBefore  *WindowBaseline `json:"fable_before,omitempty"`
	State        string          `json:"state"`
	Result       string          `json:"result,omitempty"`
	// Attempts counts submissions; it is saved before each one is sent.
	Attempts int `json:"attempts"`
	// ConfirmedAt is when the outcome was received or reconciled. Observations
	// and cooldowns up to it may predate the reset.
	ConfirmedAt time.Time `json:"confirmed_at,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Recovered marks a spent reset whose local cooldown and routing recovery
	// completed. Until then it is retried, including after a restart.
	Recovered bool `json:"recovered,omitempty"`
}

// Spent reports whether the operation consumed a reset.
func (o Operation) Spent() bool {
	return o.State == OpDone
}

type journalFile struct {
	Version    int         `json:"version"`
	Operations []Operation `json:"operations"`
}

// Journal is the durable claim log. Only the account-policy writer may save.
type Journal struct {
	mu   sync.Mutex
	path string
	ops  []Operation
}

// OpenJournal loads the journal in dir. A missing file is an empty journal; an
// unreadable one is an error, so claims fail closed.
func OpenJournal(dir string) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("claude reset journal: state directory is unknown")
	}
	j := &Journal{path: filepath.Join(dir, JournalFile)}
	raw, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claude reset journal: read: %w", err)
	}
	var file journalFile
	if errDecode := json.Unmarshal(raw, &file); errDecode != nil {
		return nil, fmt.Errorf("claude reset journal: decode: %w", errDecode)
	}
	if file.Version != journalFileVersion {
		return nil, fmt.Errorf("claude reset journal: unsupported version %d", file.Version)
	}
	j.ops = file.Operations
	return j, nil
}

// Reload re-reads the file, so a decision taken under the redemption lock sees
// every operation another process journaled. A failed reload keeps the copy.
func (j *Journal) Reload() error {
	fresh, err := OpenJournal(filepath.Dir(j.path))
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.ops = fresh.ops
	j.mu.Unlock()
	return nil
}

// Operations returns a copy, newest first.
func (j *Journal) Operations() []Operation {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := append([]Operation(nil), j.ops...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// Save durably inserts or replaces op. The in-memory copy changes only after
// the file is safely on disk.
func (j *Journal) Save(op Operation) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	next := append([]Operation(nil), j.ops...)
	replaced := false
	for i := range next {
		if next[i].ID == op.ID {
			next[i], replaced = op, true
			break
		}
	}
	if !replaced {
		next = append(next, op)
	}
	next = trim(next)
	if err := writeAtomic(j.path, journalFile{Version: journalFileVersion, Operations: next}); err != nil {
		return err
	}
	j.ops = next
	return nil
}

// trim drops the oldest settled operations beyond the size limit. Pending
// operations are never dropped.
func trim(ops []Operation) []Operation {
	if len(ops) <= maxJournalEntries {
		return ops
	}
	sort.SliceStable(ops, func(a, b int) bool { return ops[a].CreatedAt.Before(ops[b].CreatedAt) })
	out := make([]Operation, 0, maxJournalEntries)
	drop := len(ops) - maxJournalEntries
	for _, op := range ops {
		if drop > 0 && op.State != OpPending {
			drop--
			continue
		}
		out = append(out, op)
	}
	return out
}

func writeAtomic(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return fmt.Errorf("claude reset journal: encode: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+JournalFile+".*")
	if err != nil {
		return fmt.Errorf("claude reset journal: create: %w", err)
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("claude reset journal: chmod: %w", errChmod)
	}
	if _, errWrite := tmp.Write(raw); errWrite != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("claude reset journal: write: %w", errWrite)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("claude reset journal: sync: %w", errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		cleanup()
		return fmt.Errorf("claude reset journal: close: %w", errClose)
	}
	if errRename := os.Rename(name, path); errRename != nil {
		cleanup()
		return fmt.Errorf("claude reset journal: rename: %w", errRename)
	}
	d, errOpen := os.Open(dir)
	if errOpen != nil {
		return fmt.Errorf("claude reset journal: open directory: %w", errOpen)
	}
	errSync := d.Sync()
	errClose := d.Close()
	if errSync != nil {
		return fmt.Errorf("claude reset journal: sync directory: %w", errSync)
	}
	if errClose != nil {
		return fmt.Errorf("claude reset journal: close directory: %w", errClose)
	}
	return nil
}

// AccountKey identifies the logical account of an operation, so renaming or
// re-registering a credential cannot escape its quarantine.
func AccountKey(accountUUID, credentialID string) string {
	if accountUUID != "" {
		return "account:" + accountUUID
	}
	return "credential:" + credentialID
}

// mayHaveSpent reports operations that spent, or may have spent, a reset.
func (o Operation) mayHaveSpent() bool {
	switch {
	case o.State == OpDone || o.State == OpPending:
		return true
	case o.State == OpFailed:
		return o.Result == ResultUnresolved || o.Result == ResultIdentityChanged || o.Result == ResultAccountGone
	}
	return false
}

// History summarises the journal for a decision at now. Maps are keyed by
// AccountKey.
type History struct {
	LastNeedClaimAt time.Time
	NeedClaims24h   int
	BackoffUntil    map[string]time.Time
	Pending         map[string]bool
	LastClaimAt     map[string]time.Time
	// Unrecovered marks spent resets whose local recovery has not confirmed
	// the restored allowance yet.
	Unrecovered map[string]bool
}

// Summarize derives decision inputs from operations. A claim that may have
// spent a reset (pending or unresolved) counts against the settle period and
// the daily cap, so an unconfirmed reset never leads to extra spending.
func Summarize(ops []Operation, now time.Time) History {
	h := History{BackoffUntil: map[string]time.Time{}, Pending: map[string]bool{}, LastClaimAt: map[string]time.Time{}, Unrecovered: map[string]bool{}}
	for _, op := range ops {
		key := AccountKey(op.AccountUUID, op.CredentialID)
		at := op.UpdatedAt
		if op.State == OpPending {
			at = op.CreatedAt
			h.Pending[key] = true
		}
		if op.State == OpDone && !op.Recovered && now.Before(RecoveryUntil(op)) {
			h.Unrecovered[key] = true
		}
		if op.mayHaveSpent() {
			if at.After(h.LastClaimAt[key]) {
				h.LastClaimAt[key] = at
			}
			if op.Trigger == TriggerWeeklyNeed || op.Trigger == TriggerFableNeed {
				if at.After(h.LastNeedClaimAt) {
					h.LastNeedClaimAt = at
				}
				if now.Sub(at) < 24*time.Hour {
					h.NeedClaims24h++
				}
			}
		}
		if op.State == OpFailed && op.Result != ResultNotSent {
			until := op.UpdatedAt.Add(FailureBackoff)
			if until.After(h.BackoffUntil[key]) {
				h.BackoffUntil[key] = until
			}
		}
	}
	return h
}
