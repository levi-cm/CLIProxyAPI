package accountpolicy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Real adapter snapshots must be eligible and use structured IDs/types, never fixture-only statuses.
func TestCodexDiscoveryFeedsPolicyEvaluation(t *testing.T) {
	usage, credits := codexFixture(t, "usage"), codexFixture(t, "credits")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, usage)
		} else {
			fmt.Fprint(w, credits)
		}
	}))
	defer server.Close()
	snapshot, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.Enabled = true
	settings.Automation = "auto_expiring"
	// Pin local acquisition timestamps to the scenario clock while preserving every backend business field.
	now := testTime()
	snapshot.ObservedAt = now
	snapshot.InventoryObservedAt = now
	for i := range snapshot.Buckets {
		snapshot.Buckets[i].ObservedAt = now
	}
	evaluation := Evaluate(snapshot, "other-model", settings, now)
	if !evaluation.Eligible || !evaluation.Known || evaluation.Reason != "expiring_reset_credit" || !evaluation.Deadline.Equal(time.Date(2026, 10, 5, 4, 8, 0, 0, time.UTC)) {
		t.Fatalf("actual provider snapshot rejected or ignored expiry: %+v", evaluation)
	}
	if Evaluate(snapshot, "fixture-model", settings, now).Eligible {
		t.Fatal("actual provider model restriction ignored")
	}
	settings.Automation = "off"
	evaluation = Evaluate(snapshot, "other-model", settings, now)
	if !evaluation.Eligible || evaluation.Reason != "normal_weekly_reset" || !evaluation.Deadline.Equal(time.Unix(1791496800, 0)) {
		t.Fatalf("actual provider weekly observation ignored: %+v", evaluation)
	}
}
