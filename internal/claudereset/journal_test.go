package claudereset

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "op1", RequestID: "req1", CredentialID: "a", GrantID: "full", State: OpPending, CreatedAt: testNow, UpdatedAt: testNow}
	if errSave := j.Save(op); errSave != nil {
		t.Fatal(errSave)
	}
	op.State, op.Result = OpDone, "reset"
	if errSave := j.Save(op); errSave != nil {
		t.Fatal(errSave)
	}
	info, err := os.Stat(filepath.Join(dir, JournalFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal file mode = %v, err = %v", info, err)
	}
	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	ops := reopened.Operations()
	if len(ops) != 1 || ops[0].State != OpDone || ops[0].RequestID != "req1" {
		t.Fatalf("reopened = %+v", ops)
	}
}

func TestJournalCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, JournalFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(dir); err == nil {
		t.Fatal("corrupt journal opened")
	}
	if _, err := OpenJournal(""); err == nil {
		t.Fatal("journal without directory opened")
	}
}

func TestJournalTrimKeepsPending(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pending := Operation{ID: "pending", State: OpPending, CreatedAt: testNow.Add(-1000 * time.Hour)}
	if errSave := j.Save(pending); errSave != nil {
		t.Fatal(errSave)
	}
	for i := 0; i < maxJournalEntries+5; i++ {
		op := Operation{ID: time.Duration(i).String(), State: OpFailed, CreatedAt: testNow.Add(time.Duration(i) * time.Minute)}
		if errSave := j.Save(op); errSave != nil {
			t.Fatal(errSave)
		}
	}
	ops := j.Operations()
	if len(ops) != maxJournalEntries {
		t.Fatalf("len = %d", len(ops))
	}
	found := false
	for _, op := range ops {
		found = found || op.ID == "pending"
	}
	if !found {
		t.Fatal("pending operation trimmed")
	}
}

func TestSummarize(t *testing.T) {
	ops := []Operation{
		{CredentialID: "a", AccountUUID: "A", State: OpDone, Trigger: TriggerWeeklyNeed, UpdatedAt: testNow.Add(-time.Hour)},
		{CredentialID: "a", AccountUUID: "A", State: OpDone, Trigger: TriggerFableNeed, UpdatedAt: testNow.Add(-30 * time.Hour)},
		{CredentialID: "b", AccountUUID: "B", State: OpDone, Trigger: TriggerExpiry, UpdatedAt: testNow.Add(-time.Minute)},
		{CredentialID: "c", State: OpFailed, Result: "not_limited", UpdatedAt: testNow.Add(-10 * time.Minute)},
	}
	h := Summarize(ops, testNow)
	if len(h.Pending) != 0 || h.NeedClaims24h != 1 || !h.LastNeedClaimAt.Equal(testNow.Add(-time.Hour)) {
		t.Fatalf("history = %+v", h)
	}
	if !h.BackoffUntil[AccountKey("", "c")].Equal(testNow.Add(50*time.Minute)) || !h.LastClaimAt[AccountKey("B", "b")].Equal(testNow.Add(-time.Minute)) || !h.LastClaimAt[AccountKey("", "c")].IsZero() {
		t.Fatalf("history = %+v", h)
	}
	// A pending need claim quarantines its logical account, whatever the
	// credential is now called, and counts as spent.
	h = Summarize(append(ops, Operation{CredentialID: "d-renamed", AccountUUID: "D", State: OpPending, Trigger: TriggerWeeklyNeed, CreatedAt: testNow.Add(-2 * time.Minute)}), testNow)
	if !h.Pending[AccountKey("D", "anything")] || h.Pending[AccountKey("A", "a")] || h.NeedClaims24h != 2 || !h.LastNeedClaimAt.Equal(testNow.Add(-2*time.Minute)) {
		t.Fatalf("history with pending = %+v", h)
	}
	// A spent reset stays unrecovered until its recovery confirmed it.
	h = Summarize(append(ops, Operation{CredentialID: "f", AccountUUID: "F", State: OpDone, Trigger: TriggerWeeklyNeed, UpdatedAt: testNow.Add(-5 * time.Minute)}), testNow)
	if !h.Unrecovered[AccountKey("F", "f")] {
		t.Fatalf("unrecovered not reported: %+v", h)
	}
	// An unresolved claim keeps counting against the cap.
	h = Summarize(append(ops, Operation{CredentialID: "e", AccountUUID: "E", State: OpFailed, Result: ResultUnresolved, Trigger: TriggerWeeklyNeed, UpdatedAt: testNow.Add(-5 * time.Minute)}), testNow)
	if h.NeedClaims24h != 2 || !h.LastNeedClaimAt.Equal(testNow.Add(-5*time.Minute)) {
		t.Fatalf("history with unresolved = %+v", h)
	}
}
