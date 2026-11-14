// Package fleetactivity counts the agents working through the proxy, across
// every provider and client device, so subagent advice can share capacity
// between orchestrators. It keeps only hashed identities in memory: never
// prompts, bodies or keys.
package fleetactivity

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// retention bounds how far back Snapshot can look.
	retention = time.Hour
	// maxSessions bounds memory; new sessions are ignored beyond it until
	// old ones expire.
	maxSessions = 20000
	peakSlots   = 61 // one per minute, covering retention
)

// Tracker records recent sessions and in-flight requests.
type Tracker struct {
	mu       sync.Mutex
	now      func() time.Time
	seen     map[seenKey]time.Time
	inFlight map[string]int // "p:"+provider or "d:"+device
	peaks    map[string]*[peakSlots]peakSlot
	pruneAt  time.Time
}

type seenKey struct{ provider, device, session string }

type peakSlot struct {
	minute int64
	max    int
}

// New creates an empty tracker.
func New() *Tracker {
	return &Tracker{now: time.Now, seen: map[seenKey]time.Time{}, inFlight: map[string]int{}, peaks: map[string]*[peakSlots]peakSlot{}}
}

var defaultTracker = New()

// Default is the process-wide tracker fed by the request path.
func Default() *Tracker { return defaultTracker }

// SetClock replaces the clock (tests).
func (t *Tracker) SetClock(now func() time.Time) {
	t.mu.Lock()
	t.now = now
	t.mu.Unlock()
}

// Request is one in-flight request. Its provider is known once an account is
// selected; it counts as in flight from then until End.
type Request struct {
	t        *Tracker
	device   string
	session  string
	provider string
	ended    bool
}

// Begin starts tracking a request. device identifies the client (an already
// irreversible caller scope, or empty); session is the harness session ID
// (empty when unknown). Both are hashed before they are stored.
func (t *Tracker) Begin(device, session string) *Request {
	if t == nil {
		return nil
	}
	return &Request{t: t, device: DeviceRef(device), session: shortHash("session", session)}
}

// Provider records the provider serving the request; a retry on another
// provider moves the request there.
func (r *Request) Provider(provider string) {
	if r == nil {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	t := r.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.ended || provider == "" {
		return
	}
	now := t.now()
	t.pruneLocked(now)
	if r.provider != provider {
		if r.provider != "" {
			t.addLocked("p:"+r.provider, -1, now)
			t.addLocked("d:"+r.device, -1, now)
		}
		r.provider = provider
		t.addLocked("p:"+provider, 1, now)
		t.addLocked("d:"+r.device, 1, now)
	}
	if r.session != "" {
		key := seenKey{provider, r.device, r.session}
		if _, ok := t.seen[key]; ok || len(t.seen) < maxSessions {
			t.seen[key] = now
		}
	}
}

// End stops tracking the request. It is safe to call more than once.
func (r *Request) End() {
	if r == nil {
		return
	}
	t := r.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	if r.provider == "" {
		return
	}
	now := t.now()
	t.addLocked("p:"+r.provider, -1, now)
	t.addLocked("d:"+r.device, -1, now)
	if r.session != "" {
		// A long request counts as working until it ends.
		key := seenKey{r.provider, r.device, r.session}
		if _, ok := t.seen[key]; ok {
			t.seen[key] = now
		}
	}
}

func (t *Tracker) addLocked(key string, delta int, now time.Time) {
	n := t.inFlight[key] + delta
	if n <= 0 {
		delete(t.inFlight, key)
		n = 0
	} else {
		t.inFlight[key] = n
	}
	ring := t.peaks[key]
	if ring == nil {
		if n == 0 {
			return
		}
		ring = &[peakSlots]peakSlot{}
		t.peaks[key] = ring
	}
	minute := now.Unix() / 60
	slot := &ring[minute%peakSlots]
	if slot.minute != minute {
		// The level before this change held since the previous change.
		*slot = peakSlot{minute: minute, max: max(0, n-delta)}
	}
	slot.max = max(slot.max, n)
}

func (t *Tracker) pruneLocked(now time.Time) {
	if now.Before(t.pruneAt) {
		return
	}
	t.pruneAt = now.Add(time.Minute)
	for key, at := range t.seen {
		if now.Sub(at) > retention {
			delete(t.seen, key)
		}
	}
	oldest := now.Add(-retention).Unix() / 60
	for key, ring := range t.peaks {
		live := t.inFlight[key] > 0
		for _, s := range ring {
			live = live || (s.max > 0 && s.minute >= oldest)
		}
		if !live {
			delete(t.peaks, key)
		}
	}
}

// Group is the activity of one provider or device.
type Group struct {
	// Agents is the larger of the distinct sessions seen in the window and
	// the peak in-flight requests: harnesses whose subagents share the parent
	// session (Claude Code) still count one agent per concurrent request.
	Agents       int `json:"agents"`
	Sessions     int `json:"sessions"`
	InFlight     int `json:"in_flight"`
	PeakInFlight int `json:"peak_in_flight"`
}

// Snapshot is the fleet activity over a sliding window.
type Snapshot struct {
	WindowSeconds int              `json:"window_seconds"`
	Agents        int              `json:"agents"`
	InFlight      int              `json:"in_flight"`
	Providers     map[string]Group `json:"providers"`
	Devices       map[string]Group `json:"devices"`
}

// Snapshot counts agents working within window.
func (t *Tracker) Snapshot(window time.Duration) Snapshot {
	out := Snapshot{WindowSeconds: int(window / time.Second), Providers: map[string]Group{}, Devices: map[string]Group{}}
	if t == nil {
		return out
	}
	window = min(max(window, time.Minute), retention)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	since := now.Add(-window)
	type pair struct{ group, session string }
	sessions := map[pair]bool{}
	for key, at := range t.seen {
		if at.Before(since) {
			continue
		}
		sessions[pair{"p:" + key.provider, key.provider + "\x00" + key.session}] = true
		sessions[pair{"d:" + key.device, key.provider + "\x00" + key.session}] = true
	}
	groups := map[string]*Group{}
	get := func(key string) *Group {
		g := groups[key]
		if g == nil {
			g = &Group{}
			groups[key] = g
		}
		return g
	}
	for p := range sessions {
		get(p.group).Sessions++
	}
	first := since.Unix() / 60
	for key, ring := range t.peaks {
		g := get(key)
		g.InFlight = t.inFlight[key]
		g.PeakInFlight = g.InFlight
		for _, s := range ring {
			if s.minute >= first {
				g.PeakInFlight = max(g.PeakInFlight, s.max)
			}
		}
	}
	for key, g := range groups {
		g.Agents = max(g.Sessions, g.PeakInFlight)
		if g.Agents == 0 {
			continue
		}
		name := key[2:]
		if strings.HasPrefix(key, "p:") {
			out.Providers[name] = *g
			out.Agents += g.Agents
			out.InFlight += g.InFlight
		} else {
			out.Devices[name] = *g
		}
	}
	return out
}

// DeviceRef shortens a caller scope to a stable, non-reversible device label.
func DeviceRef(callerScope string) string {
	callerScope = strings.TrimSpace(callerScope)
	if callerScope == "" {
		return "no-key"
	}
	return "key-" + shortHash("device", callerScope)[:6]
}

func shortHash(kind, v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("fleetactivity:" + kind + "\x00" + v))
	return hex.EncodeToString(sum[:8])
}

// SortedNames returns map keys in order (for stable text output).
func SortedNames(groups map[string]Group) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
