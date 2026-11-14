package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/fleetactivity"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// The request lease feeds the fleet-wide agent count: the harness session
// header identifies the agent, the caller scope the device, and the selected
// account the provider. Per-account ActiveRequests accounting is unchanged.
func TestPolicyLeaseFeedsFleetActivity(t *testing.T) {
	const provider = "fleet-test-provider"
	m := NewManager(nil, nil, nil)
	start := func(session string) *policyRequestLease {
		headers := http.Header{}
		headers.Set("X-Claude-Code-Session-Id", session)
		opts := cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "scope-1"}}
		opts, lease := m.trackPolicyRequest(context.Background(), opts)
		_, opts = cliproxysession.Enrich(cliproxyexecutor.Request{Payload: []byte(`{"messages":[]}`)}, opts)
		lease.trackActivity(opts)
		if err := beginPolicyAttempt(context.Background(), opts, "acct-1", provider); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	a := start("11111111-1111-4111-8111-111111111111")
	b := start("11111111-1111-4111-8111-111111111111")
	c := start("22222222-2222-4222-8222-222222222222")
	g := fleetactivity.Default().Snapshot(10 * time.Minute).Providers[provider]
	if g.Sessions != 2 || g.InFlight != 3 || g.Agents != 3 {
		t.Fatalf("activity = %+v", g)
	}
	if m.PolicyRuntime("acct-1").ActiveRequests != 3 {
		t.Fatalf("active requests = %d", m.PolicyRuntime("acct-1").ActiveRequests)
	}
	for _, l := range []*policyRequestLease{a, b, c} {
		l.release()
		l.release()
	}
	g = fleetactivity.Default().Snapshot(10 * time.Minute).Providers[provider]
	if g.InFlight != 0 || g.Sessions != 2 || m.PolicyRuntime("acct-1").ActiveRequests != 0 {
		t.Fatalf("after release = %+v, active %d", g, m.PolicyRuntime("acct-1").ActiveRequests)
	}
	if _, ok := fleetactivity.Default().Snapshot(10 * time.Minute).Devices[fleetactivity.DeviceRef("scope-1")]; !ok {
		t.Fatal("device missing")
	}
}
