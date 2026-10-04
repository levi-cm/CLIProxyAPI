package accountpolicy

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func unresolved(state string) bool {
	return state == "planned" || state == "prepared" || state == "submitted" || state == "verifying" || state == "outcome_unknown"
}
func (s *Service) saveOperation(op Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := clone(s.state)
	op.UpdatedAt = s.now()
	for i, old := range s.state.Operations {
		if old.ID == op.ID {
			s.state.Operations[i] = clone(op)
			return s.persistLocked(before)
		}
	}
	s.state.Operations = append(s.state.Operations, clone(op))
	return s.persistLocked(before)
}
func changedAllowance(before, after Snapshot) bool {
	for _, b := range before.Buckets {
		for _, a := range after.Buckets {
			if a.Scope == b.Scope && a.Model == b.Model && a.DurationSeconds == b.DurationSeconds && (a.UsedPercent != b.UsedPercent || !a.ResetAt.Equal(b.ResetAt) || (a.Allowed != nil && b.Allowed != nil && *a.Allowed != *b.Allowed)) {
				return true
			}
		}
	}
	return false
}
func weeklyExhausted(s Snapshot) bool {
	for _, b := range s.Buckets {
		// Allowed is an aggregate scope permission; denial alone cannot identify an exhausted window.
		if b.Scope == "ordinary" && b.DurationSeconds == 604800 && b.UsedPercent >= 100 && (b.Allowed == nil || !*b.Allowed) {
			return true
		}
	}
	return false
}

func sameCreditEvidence(a, b Credit) bool {
	if a.ID != b.ID || a.Type != b.Type || a.Status != b.Status || a.DetailsKnown != b.DetailsKnown || !a.GrantedAt.Equal(b.GrantedAt) || (a.ExpiresAt == nil) != (b.ExpiresAt == nil) {
		return false
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.Equal(*b.ExpiresAt) {
		return false
	}
	as, bs := slices.Clone(a.Scopes), slices.Clone(b.Scopes)
	sort.Strings(as)
	sort.Strings(bs)
	return slices.Equal(as, bs)
}
func invalidatedCredit(op Operation, snapshot Snapshot) bool {
	if op.State != "no_credit" || op.Before == nil {
		return false
	}
	before, ok := findCredit(*op.Before, op.CreditID)
	if !ok {
		return true
	}
	after, ok := findCredit(snapshot, op.CreditID)
	return !ok || sameCreditEvidence(before, after)
}
func permanentConsumeFailure(err error) string {
	var coded interface{ FailureCode() string }
	if !errors.As(err, &coded) {
		return ""
	}
	code := coded.FailureCode()
	if strings.Contains(code, "schema") || strings.Contains(code, "identity") || code == "workspace_identity_unsupported" || code == "automatic_writes_disabled" || code == "authentication_rejected" {
		return code
	}
	return ""
}
func (s *Service) disableWrites(id, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.WriteFailures == nil {
		s.state.WriteFailures = map[string]string{}
	}
	s.state.WriteFailures[id] = code
	snapshot := s.state.Snapshots[id]
	snapshot.WritesDisabled = true
	snapshot.LastError = code
	s.state.Snapshots[id] = snapshot
}

func naturalWeeklyReset(s Snapshot, now time.Time) time.Time {
	var deadline time.Time
	for _, bucket := range s.Buckets {
		if bucket.Scope == "ordinary" && bucket.DurationSeconds == 604800 && bucket.ResetAt.After(now) && (deadline.IsZero() || bucket.ResetAt.Before(deadline)) {
			deadline = bucket.ResetAt
		}
	}
	return deadline
}
func coveredUsed(s Snapshot) bool {
	for _, b := range s.Buckets {
		if b.Scope == "ordinary" && b.UsedPercent > 0 {
			return true
		}
	}
	return false
}
func confirmedRecovery(op Operation, after Snapshot) bool {
	if op.Before == nil {
		return false
	}
	before := clone(*op.Before)
	before.Identity.Generation = after.Identity.Generation
	if !recovered(before, after) {
		quotaChanged := false
		if (op.Result == "reset" || op.Result == "already_redeemed") && sameIdentity(before.Identity, after.Identity) && !after.ObservedAt.Before(before.ObservedAt) {
			for _, old := range before.Buckets {
				if old.Scope != "ordinary" {
					continue
				}
				for _, next := range after.Buckets {
					if next.Scope == old.Scope && next.Model == old.Model && next.DurationSeconds == old.DurationSeconds && !next.ObservedAt.Before(old.ObservedAt) && next.Allowed != nil && *next.Allowed && next.UsedPercent < old.UsedPercent {
						quotaChanged = true
					}
				}
			}
		}
		if !quotaChanged {
			return false
		}
	}
	credit, found := findCredit(after, op.CreditID)
	return after.InventoryComplete && fresh(after.InventoryObservedAt, after.ObservedAt, 120) && (!found || credit.Status != "available")
}

// Redeem requires explicit account and credit IDs. Repeated uncertain writes reuse one durable request ID.
func (s *Service) Redeem(ctx context.Context, id, creditID string) (Operation, error) {
	return s.redeem(ctx, id, creditID, nil)
}

func (s *Service) redeem(ctx context.Context, id, creditID string, expectedOwner *Identity) (Operation, error) {
	if id == "" || creditID == "" {
		return Operation{}, policyError("unknown_credit", "explicit account and selected credit are required")
	}
	settings := s.Settings()
	if !settings.Enabled {
		return Operation{}, policyError("disabled", "account policy is disabled")
	}
	if settings.ReadOnly {
		return Operation{}, policyError("read_only", "reset automation is read-only")
	}
	if s.stateDir == "" {
		return Operation{}, policyError("persistence", "durable journal is required for reset writes")
	}
	identity, err := s.identity(id)
	if err != nil {
		return Operation{}, err
	}
	if expectedOwner != nil && !sameOwnership(*expectedOwner, identity) {
		return Operation{}, policyError("identity_mismatch", "scheduled reset account ownership changed")
	}
	unlock, err := s.accountLock(ctx, identity)
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	if s.opts.AcquireReset != nil {
		release, errAcquire := s.opts.AcquireReset(ctx, id)
		if errAcquire != nil {
			return Operation{}, policyError("account_busy", "active inference prevents reset")
		}
		defer release()
	}
	unlockJournal, err := lockJournal(s.stateDir)
	if err != nil {
		return Operation{}, err
	}
	defer unlockJournal()
	// Re-read durable operations under the shared process lock. A second proxy cannot lose a pending operation.
	if err = s.reloadOperations(); err != nil {
		return Operation{}, err
	}
	var op Operation
	for _, old := range s.Operations() {
		if old.CredentialID == id && unresolved(old.State) {
			if old.CreditID != creditID {
				return old, policyError("conflict", "another redemption is unresolved")
			}
			op = old
			break
		}
	}
	snapshot, err := s.discoverLocked(ctx, identity)
	if err != nil {
		return op, err
	}
	if identity.AccountID == "" || identity.WorkspaceID == "" || snapshot.WritesDisabled || !sameIdentity(identity, snapshot.Identity) {
		return op, policyError("identity_mismatch", "verified account and workspace identity are required")
	}
	if current, errCurrent := s.identity(id); errCurrent != nil || !sameIdentity(identity, current) {
		return op, policyError("identity_mismatch", "credential changed during reset preparation")
	}
	settings = s.Settings()
	if !settings.Enabled || settings.ReadOnly {
		return op, policyError("read_only", "reset writes are disabled")
	}
	if op.ID != "" {
		beforeIdentity := Identity{}
		if op.Before != nil {
			beforeIdentity = op.Before.Identity
			beforeIdentity.Generation = identity.Generation
		}
		if op.AccountID != identity.AccountID || op.WorkspaceID != identity.WorkspaceID || op.Before == nil || !sameIdentity(beforeIdentity, identity) {
			return op, policyError("identity_mismatch", "pending operation belongs to a different credential generation")
		}
		if confirmedRecovery(op, snapshot) {
			op.State = "confirmed"
			op.Error = ""
			op.After = &snapshot
			return op, s.saveOperation(op)
		}
	} else {
		credit, found := findCredit(snapshot, creditID)
		if !found {
			return Operation{}, policyError("unknown_credit", "selected credit not found")
		}
		if credit.ExpiresAt != nil && !credit.ExpiresAt.After(s.now()) {
			return Operation{}, policyError("expired", "selected credit expired")
		}
		if !fresh(snapshot.ObservedAt, s.now(), settings.FreshnessSeconds) || !fresh(snapshot.InventoryObservedAt, s.now(), settings.FreshnessSeconds) {
			return Operation{}, policyError("stale_evidence", "fresh usage and inventory are required")
		}
		if !usableCredit(credit, settings, s.now()) {
			return Operation{}, policyError("unknown_credit", "selected credit is unavailable or unsupported")
		}
		for _, old := range s.Operations() {
			if old.CredentialID != id {
				continue
			}
			if old.CreditID == creditID && invalidatedCredit(old, snapshot) {
				return old, policyError("conflict", "selected credit remains invalidated by provider no_credit result")
			}
			if old.State == "confirmed" && old.After != nil && (!snapshot.ObservedAt.After(old.After.ObservedAt) || !weeklyExhausted(snapshot)) {
				return old, policyError("conflict", "renewed depletion evidence is required before another reset")
			}
			if old.CreditID == creditID && old.State == "nothing_to_reset" && old.Before != nil && !changedAllowance(*old.Before, snapshot) {
				return old, policyError("conflict", "allowance evidence has not changed since nothing_to_reset")
			}
		}
		op = Operation{ID: uuid.NewString(), RequestID: uuid.NewString(), CredentialID: id, AccountID: identity.AccountID, WorkspaceID: identity.WorkspaceID, CreditID: creditID, State: "prepared", CreatedAt: s.now(), UpdatedAt: s.now(), Before: &snapshot}
		if err = s.saveOperation(op); err != nil {
			return op, err
		}
	}
	if s.opts.IsIdle == nil || !s.opts.IsIdle(id) || snapshot.ActiveRequests > 0 {
		return op, policyError("conflict", "account must be idle before reset submission")
	}
	op.State = "submitted"
	if err = s.saveOperation(op); err != nil {
		return op, err
	}
	result, errConsume := s.opts.Provider.Consume(ctx, identity, op.RequestID, creditID)
	if errConsume != nil {
		if code := permanentConsumeFailure(errConsume); code != "" {
			s.disableWrites(id, code)
		}
		op.State = "outcome_unknown"
		op.Error = "provider consume outcome unknown"
		delay := 30 * time.Second
		var retry interface{ RetryDelay() time.Duration }
		if errors.As(errConsume, &retry) && retry.RetryDelay() > delay {
			delay = retry.RetryDelay()
		}
		s.mu.Lock()
		if s.state.RetryAt == nil {
			s.state.RetryAt = map[string]time.Time{}
		}
		s.state.RetryAt[op.ID] = s.now().Add(delay)
		s.mu.Unlock()
		if err = s.saveOperation(op); err != nil {
			return op, err
		}
		return op, policyError("provider_error", "provider consume outcome unknown")
	}
	op.Result = result.Code
	op.Error = ""
	s.mu.Lock()
	delete(s.state.RetryAt, op.ID)
	s.mu.Unlock()
	switch result.Code {
	case "nothing_to_reset", "no_credit":
		op.State = result.Code
		if err = s.saveOperation(op); err != nil {
			return op, err
		}
		after, errRead := s.discoverLocked(ctx, identity)
		if errRead == nil {
			op.After = &after
			err = s.saveOperation(op)
		}
		return op, err
	case "reset", "already_redeemed":
		op.State = "verifying"
	default:
		op.State = "outcome_unknown"
		op.Error = "unrecognized provider consume result"
		s.disableWrites(id, "consume_schema_changed")
		_ = s.saveOperation(op)
		return op, policyError("provider_error", "unrecognized provider consume result")
	}
	if err = s.saveOperation(op); err != nil {
		return op, err
	}
	after, errRead := s.discoverLocked(ctx, identity)
	if errRead != nil {
		op.State = "outcome_unknown"
		op.Error = "post-reset verification failed"
		_ = s.saveOperation(op)
		return op, errRead
	}
	op.After = &after
	if confirmedRecovery(op, after) {
		op.State = "confirmed"
	} else {
		op.State = "outcome_unknown"
		op.Error = "provider reset requires further reconciliation"
	}
	return op, s.saveOperation(op)
}

func (s *Service) Tick(ctx context.Context) error {
	if !s.Settings().Enabled {
		return nil
	}
	identities := s.opts.Accounts()
	jobs := make(chan Identity)
	results := make(chan error, len(identities))
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for identity := range jobs {
				results <- s.tickAccount(ctx, identity)
			}
		}()
	}
	for _, identity := range identities {
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
	close(results)
	var errs []error
	for err := range results {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) tickAccount(ctx context.Context, account Identity) error {
	if !s.Settings().Enabled {
		return nil
	}
	now := s.now()
	var errs []error
	var due []Identity
	for _, identity := range []Identity{account} {
		s.mu.RLock()
		tracking, known := s.discovery[identity.CredentialID]
		s.mu.RUnlock()
		s.refreshMu.Lock()
		requested := s.requested[identity.CredentialID] + s.requested[""]
		s.refreshMu.Unlock()
		if !known || !sameOwnership(tracking.Identity, identity) || tracking.Requested != requested || !tracking.Next.After(now) {
			due = append(due, identity)
		}
	}
	jobs := make(chan Identity)
	results := make(chan error, len(due))
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for identity := range jobs {
				results <- s.Refresh(ctx, identity.CredentialID)
			}
		}()
	}
	for _, id := range due {
		select {
		case jobs <- id:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			errs = append(errs, err)
		}
	}
	settings := s.Settings()
	if !settings.Enabled || settings.ReadOnly {
		return errors.Join(errs...)
	}
	for _, sch := range s.Schedules() {
		if sch.CredentialID != account.CredentialID {
			continue
		}
		if sch.At.After(now) {
			continue
		}
		if sch.AccountID == "" || sch.WorkspaceID == "" || sch.Provider == "" || sch.AccountID != account.AccountID || sch.WorkspaceID != account.WorkspaceID || sch.Provider != account.Provider {
			op := Operation{ID: uuid.NewString(), RequestID: uuid.NewString(), CredentialID: sch.CredentialID, AccountID: sch.AccountID, WorkspaceID: sch.WorkspaceID, CreditID: sch.CreditID, State: "failed", Result: "identity_mismatch", Error: "scheduled account ownership changed", CreatedAt: now, UpdatedAt: now}
			if err := s.saveOperation(op); err != nil {
				errs = append(errs, err)
			} else if err := s.CancelSchedule(sch.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if snapshot, ok := s.Snapshot(sch.CredentialID); ok {
			if credit, found := findCredit(snapshot, sch.CreditID); found && credit.ExpiresAt != nil && !credit.ExpiresAt.After(now) {
				op := Operation{ID: uuid.NewString(), RequestID: uuid.NewString(), CredentialID: sch.CredentialID, AccountID: snapshot.Identity.AccountID, WorkspaceID: snapshot.Identity.WorkspaceID, CreditID: sch.CreditID, State: "expired", CreatedAt: now, UpdatedAt: now, Before: &snapshot}
				if err := s.saveOperation(op); err != nil {
					errs = append(errs, err)
				} else {
					if err := s.CancelSchedule(sch.ID); err != nil {
						errs = append(errs, err)
					}
				}
				continue
			}
		}
		owner := Identity{CredentialID: sch.CredentialID, AccountID: sch.AccountID, WorkspaceID: sch.WorkspaceID, Provider: sch.Provider}
		if _, err := s.redeem(ctx, sch.CredentialID, sch.CreditID, &owner); err != nil {
			errs = append(errs, err)
		} else {
			_ = s.CancelSchedule(sch.ID)
		}
	}
	if settings.Automation != "auto_expiring" && settings.Automation != "auto_all_saved" {
		return errors.Join(errs...)
	}
	for _, snapshot := range s.Accounts() {
		if snapshot.Identity.CredentialID != account.CredentialID {
			continue
		}
		id := snapshot.Identity.CredentialID
		if settings.Accounts[id].Hold || snapshot.WritesDisabled || snapshot.LastError != "" || !fresh(snapshot.ObservedAt, now, settings.FreshnessSeconds) || !fresh(snapshot.InventoryObservedAt, now, settings.FreshnessSeconds) {
			continue
		}
		pending := ""
		waitPending := false
		for _, op := range s.Operations() {
			if op.CredentialID == id && unresolved(op.State) {
				pending = op.CreditID
				waitPending = op.State == "outcome_unknown" && now.Sub(op.UpdatedAt) < 30*time.Second
				s.mu.RLock()
				retryAt := s.state.RetryAt[op.ID]
				s.mu.RUnlock()
				waitPending = waitPending || retryAt.After(now)
				break
			}
		}
		if pending != "" {
			if waitPending {
				continue
			}
			if _, err := s.Redeem(ctx, id, pending); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		credits := []Credit{}
		for _, credit := range snapshot.Credits {
			if usableCredit(credit, settings, now) {
				credits = append(credits, credit)
			}
		}
		sort.Slice(credits, func(i, j int) bool {
			if credits[i].ExpiresAt == nil {
				return false
			}
			if credits[j].ExpiresAt == nil {
				return true
			}
			if credits[i].ExpiresAt.Equal(*credits[j].ExpiresAt) {
				return credits[i].ID < credits[j].ID
			}
			return credits[i].ExpiresAt.Before(*credits[j].ExpiresAt)
		})
		for _, credit := range credits {
			blocked := false
			for _, op := range s.Operations() {
				if op.CredentialID != id {
					continue
				}
				if op.CreditID == credit.ID && op.State == "nothing_to_reset" && op.Before != nil && !changedAllowance(*op.Before, snapshot) {
					blocked = true
				}
				if op.CreditID == credit.ID && invalidatedCredit(op, snapshot) {
					blocked = true
				}
				if op.State == "confirmed" && op.After != nil && (!snapshot.ObservedAt.After(op.After.ObservedAt) || !weeklyExhausted(snapshot)) {
					blocked = true
				}
			}
			if blocked {
				continue
			}
			demand := s.opts.HasDemand != nil && s.opts.HasDemand(id)
			due := credit.ExpiresAt != nil && !credit.ExpiresAt.Add(-time.Duration(settings.ExpiryGuardSeconds)*time.Second).After(now)
			// Expiring allowance is refreshed immediately after weekly depletion when the benefit
			// would otherwise expire before natural renewal. This does not require queued work.
			earlyExpiring := weeklyExhausted(snapshot) && credit.ExpiresAt != nil && naturalWeeklyReset(snapshot, now).After(*credit.ExpiresAt)
			earlySaved := credit.ExpiresAt == nil && settings.Automation == "auto_all_saved" && weeklyExhausted(snapshot) && demand
			if credit.ExpiresAt == nil && (settings.Automation != "auto_all_saved" || !snapshot.InventoryComplete || snapshot.AvailableCredits <= settings.SavedCreditReserve) {
				continue
			}
			if !earlyExpiring && !earlySaved && !(due && coveredUsed(snapshot)) {
				continue
			}
			if _, err := s.Redeem(ctx, id, credit.ID); err != nil {
				errs = append(errs, err)
			}
			break
		}
	}
	return errors.Join(errs...)
}
