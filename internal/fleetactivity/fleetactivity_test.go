package fleetactivity

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func newTracker() (*Tracker, *time.Time) {
	tr := New()
	now := t0
	tr.SetClock(func() time.Time { return now })
	return tr, &now
}

func TestSessionsCountAcrossProvidersAndDevices(t *testing.T) {
	tr, _ := newTracker()
	for _, c := range []struct{ device, session, provider string }{
		{"scope-a", "s1", "claude"}, {"scope-a", "s1", "claude"}, {"scope-a", "s2", "codex"},
		{"scope-b", "s3", "codex"}, {"scope-b", "s4", "codex"},
	} {
		r := tr.Begin(c.device, c.session)
		r.Provider(c.provider)
		r.End()
	}
	s := tr.Snapshot(10 * time.Minute)
	if s.Agents != 4 || s.Providers["claude"].Agents != 1 || s.Providers["codex"].Agents != 3 || s.InFlight != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	a, b := DeviceRef("scope-a"), DeviceRef("scope-b")
	if s.Devices[a].Agents != 2 || s.Devices[b].Agents != 2 || len(s.Devices) != 2 {
		t.Fatalf("devices = %+v", s.Devices)
	}
	if strings.Contains(a, "scope") || !strings.HasPrefix(a, "key-") || len(a) != 10 {
		t.Fatalf("device ref leaks or is malformed: %q", a)
	}
}

func TestSharedSessionCountsConcurrencyAsFloor(t *testing.T) {
	tr, now := newTracker()
	// A Claude Code parent and three subagents share one session ID.
	var open []*Request
	for i := 0; i < 4; i++ {
		r := tr.Begin("scope", "shared")
		r.Provider("claude")
		open = append(open, r)
	}
	s := tr.Snapshot(10 * time.Minute)
	if g := s.Providers["claude"]; g.Sessions != 1 || g.InFlight != 4 || g.Agents != 4 {
		t.Fatalf("in flight = %+v", g)
	}
	*now = now.Add(3 * time.Minute)
	for _, r := range open {
		r.End()
		r.End() // idempotent
	}
	if g := tr.Snapshot(10 * time.Minute).Providers["claude"]; g.InFlight != 0 || g.PeakInFlight != 4 || g.Agents != 4 {
		t.Fatalf("after end = %+v", g)
	}
	// The peak leaves the window; the session does too.
	*now = now.Add(11 * time.Minute)
	if s := tr.Snapshot(10 * time.Minute); s.Agents != 0 || len(s.Providers) != 0 {
		t.Fatalf("expired = %+v", s)
	}
}

func TestLongSteadyConcurrencyStaysVisible(t *testing.T) {
	tr, now := newTracker()
	var open []*Request
	for i := 0; i < 3; i++ {
		r := tr.Begin("scope", "")
		r.Provider("codex")
		open = append(open, r)
	}
	// Twenty quiet minutes, then the requests finish one by one.
	*now = now.Add(20 * time.Minute)
	open[0].End()
	if g := tr.Snapshot(10 * time.Minute).Providers["codex"]; g.PeakInFlight != 3 || g.Agents != 3 {
		t.Fatalf("steady concurrency lost: %+v", g)
	}
}

func TestProviderMoveAndUnselectedRequests(t *testing.T) {
	tr, _ := newTracker()
	r := tr.Begin("scope", "s1")
	r.Provider("claude")
	r.Provider("codex")
	s := tr.Snapshot(10 * time.Minute)
	if s.Providers["claude"].InFlight != 0 || s.Providers["codex"].InFlight != 1 {
		t.Fatalf("retry on another provider = %+v", s.Providers)
	}
	r.End()
	// A request that never reached an account counts nowhere.
	tr.Begin("scope", "s2").End()
	var nilReq *Request
	nilReq.Provider("claude")
	nilReq.End()
	if s := tr.Snapshot(10 * time.Minute); s.Providers["codex"].Sessions != 1 || s.Agents != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestMemoryIsBounded(t *testing.T) {
	tr, now := newTracker()
	for i := 0; i < maxSessions+50; i++ {
		r := tr.Begin("scope", "s"+time.Duration(i).String())
		r.Provider("codex")
		r.End()
	}
	if n := len(tr.seen); n != maxSessions {
		t.Fatalf("sessions kept = %d", n)
	}
	*now = now.Add(2 * time.Hour)
	r := tr.Begin("scope", "late")
	r.Provider("codex")
	r.End()
	if n := len(tr.seen); n != 1 {
		t.Fatalf("expired sessions kept: %d", n)
	}
}
