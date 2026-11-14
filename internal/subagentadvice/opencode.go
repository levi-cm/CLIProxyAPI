package subagentadvice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenCodeProvider is the provider whose quota comes from the usage tracker.
const OpenCodeProvider = "opencode-go"

// openCodeMaxAge is how long a tracker reading stays usable; the sampler
// refreshes it every few minutes.
const openCodeMaxAge = 15 * time.Minute

// OpenCodeWindow is one OpenCode Go usage window from the tracker.
type OpenCodeWindow struct {
	Window      string     `json:"window"`
	LeftPercent float64    `json:"left_percent"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	// PacePercentPerHour is the tracker's recent burn rate; nil when unknown.
	PacePercentPerHour *float64 `json:"pace_percent_per_hour,omitempty"`
	Verdict            string   `json:"verdict,omitempty"`
	Fresh              bool     `json:"fresh"`
}

// OpenCodeUsage is the OpenCode Go quota behind a response.
type OpenCodeUsage struct {
	// Reachable is false when the tracker is not configured or its last
	// reading failed or is too old; the budget is then unknown.
	Reachable  bool             `json:"reachable"`
	ObservedAt *time.Time       `json:"observed_at,omitempty"`
	Windows    []OpenCodeWindow `json:"windows,omitempty"`
	// AvoidUntil is set while a fresh window is used up, nearly empty, or
	// projected to run out before it resets.
	AvoidUntil *time.Time `json:"avoid_until,omitempty"`
	AvoidWhy   string     `json:"avoid_reason,omitempty"`
}

// openCodeReading is the last tracker fetch.
type openCodeReading struct {
	at      time.Time
	windows []OpenCodeWindow
}

// fetchOpenCode reads the tracker. It sets no timeout: the caller runs it in
// its own goroutine and a reading older than openCodeMaxAge counts as
// unreachable, so a hung fetch only delays fresh data.
func fetchOpenCode(ctx context.Context, client *http.Client, url string, now time.Time) (openCodeReading, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return openCodeReading{}, fmt.Errorf("build tracker request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return openCodeReading{}, fmt.Errorf("fetch tracker: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return openCodeReading{}, fmt.Errorf("read tracker: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return openCodeReading{}, fmt.Errorf("tracker returned HTTP %d", resp.StatusCode)
	}
	windows, err := parseOpenCode(body, now)
	if err != nil {
		return openCodeReading{}, err
	}
	return openCodeReading{at: now, windows: windows}, nil
}

// parseOpenCode reads opencode.windows[] from the tracker's JSON. Numbers may
// arrive as numbers or numeric strings.
func parseOpenCode(body []byte, now time.Time) ([]OpenCodeWindow, error) {
	var doc struct {
		OpenCode struct {
			Windows []map[string]any `json:"windows"`
		} `json:"opencode"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode tracker: %w", err)
	}
	if len(doc.OpenCode.Windows) == 0 {
		return nil, fmt.Errorf("tracker reported no opencode windows")
	}
	var out []OpenCodeWindow
	for _, raw := range doc.OpenCode.Windows {
		w := OpenCodeWindow{Window: anyString(raw["window"]), Verdict: strings.ToLower(anyString(raw["verdict"]))}
		left, ok := anyNumber(raw["left_pct"])
		if !ok || w.Window == "" {
			continue
		}
		w.LeftPercent = clamp(left, 0, 100)
		if hours, ok := anyNumber(raw["reset_h"]); ok && hours >= 0 {
			at := now.Add(time.Duration(hours * float64(time.Hour))).Truncate(time.Minute)
			w.ResetAt = &at
		}
		if pace, ok := anyNumber(raw["pace_pct_h"]); ok && pace >= 0 {
			w.PacePercentPerHour = &pace
		}
		w.Fresh = strings.EqualFold(anyString(raw["data"]), "fresh")
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tracker windows carry no usable left_pct")
	}
	return out, nil
}

func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case nil:
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func anyNumber(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(x), "%"), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// openCodeUsage judges the last reading at now.
func openCodeUsage(cfg Config, reading openCodeReading, now time.Time) *OpenCodeUsage {
	if cfg.OpenCodeUsageURL == "" {
		return nil
	}
	u := &OpenCodeUsage{}
	if reading.at.IsZero() || now.Sub(reading.at) > openCodeMaxAge {
		return u
	}
	at := reading.at
	u.Reachable, u.ObservedAt = true, &at
	u.Windows = append([]OpenCodeWindow(nil), reading.windows...)
	for _, w := range u.Windows {
		if !w.Fresh {
			continue
		}
		var why string
		hoursLeft := math.Inf(1)
		if w.ResetAt != nil {
			hoursLeft = w.ResetAt.Sub(now).Hours()
		}
		switch {
		case w.Verdict == "used_up" || w.LeftPercent <= 0:
			why = "used up"
		case w.LeftPercent <= cfg.OpenCodeAvoidPercent:
			why = fmt.Sprintf("%.0f%% left", w.LeftPercent)
		case w.Verdict == "too_fast" || (w.PacePercentPerHour != nil && *w.PacePercentPerHour > 0 && w.LeftPercent / *w.PacePercentPerHour < hoursLeft):
			why = fmt.Sprintf("%.0f%% left, runs out before it resets at this pace", w.LeftPercent)
		default:
			continue
		}
		why = w.Window + " " + why
		// Avoid until every blocking window has reset.
		if w.ResetAt != nil && (u.AvoidUntil == nil || w.ResetAt.After(*u.AvoidUntil)) {
			until := *w.ResetAt
			u.AvoidUntil = &until
		}
		if u.AvoidWhy == "" {
			u.AvoidWhy = why
		} else {
			u.AvoidWhy += "; " + why
		}
	}
	if u.AvoidWhy != "" && u.AvoidUntil == nil {
		// No reset time: avoid until the tracker says otherwise.
		until := now.Add(openCodeMaxAge)
		u.AvoidUntil = &until
	}
	sort.SliceStable(u.Windows, func(i, j int) bool { return windowRank(u.Windows[i].Window) < windowRank(u.Windows[j].Window) })
	return u
}

func windowRank(name string) int {
	switch strings.ToLower(name) {
	case "5h":
		return 0
	case "weekly":
		return 1
	case "monthly":
		return 2
	}
	return 3
}
