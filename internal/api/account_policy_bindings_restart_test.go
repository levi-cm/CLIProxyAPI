package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Exercise real HTTP/SSE handlers and production Codex executors. A reconstructed
// selector has no memory cache, so only the real durable file can retain B.
func TestAccountPolicyDurableHTTPContinuationAndForkAfterRestart(t *testing.T) {
	for _, stream := range []string{"false", "true"} {
		t.Run("stream_"+stream, func(t *testing.T) {
			h := newPolicyTransportHarness(t, nil)
			dir := t.TempDir()
			h.source.settings.StateDir = dir
			for _, id := range []string{h.a, h.b} {
				a, _ := h.manager.GetByID(id)
				snapshot, _ := h.source.Snapshot(id)
				a.Metadata = map[string]any{"account_id": snapshot.Identity.AccountID, "workspace_id": snapshot.Identity.AccountID}
				if _, err := h.manager.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
				h.source.update(id, func(s *accountpolicy.Snapshot) { s.Identity.WorkspaceID = s.Identity.AccountID })
			}
			install := func() {
				s := auth.NewEarliestDeadlineSelector(h.source, &auth.FillFirstSelector{}, func() time.Time { return h.now })
				s.SetIdleCheck(h.manager.PolicyIsIdle)
				s.SetBindingAuthResolver(h.manager.GetByID)
				h.manager.SetSelector(s)
				t.Cleanup(s.Stop)
			}
			install()
			parent := "019c7714-3b77-74d1-9866-e1f484aae2ab"
			child := "019c7714-3b77-74d1-9866-e1f484aae2ac"
			headers := http.Header{"Session_id": {parent}}
			status, body := h.httpRequest(t, "/v1/responses", `{`+policyTransportInput+`,"stream":`+stream+`}`, headers)
			if status != 200 || !strings.Contains(body, "resp_b") {
				t.Fatalf("initial=%d %s", status, body)
			}
			data, err := os.ReadFile(filepath.Join(dir, "conversation-bindings.json"))
			if err != nil {
				t.Fatalf("real completion did not persist: %v", err)
			}
			if strings.Contains(string(data), "test-key") || strings.Contains(string(data), parent) {
				t.Fatal("raw caller or thread persisted")
			}
			h.source.update(h.a, func(s *accountpolicy.Snapshot) { s.Buckets[1].ResetAt = h.now.Add(30 * time.Minute) })
			install() // Reconstruct all routing state; A now wins any unsafe fallback.
			continuation := `{"model":"gpt-5-codex","stream":` + stream + `,"input":[{"type":"reasoning","encrypted_content":"opaque-b"},{"role":"user","content":"continue"}]}`
			status, body = h.httpRequest(t, "/v1/responses", continuation, headers)
			if status != 200 || !strings.Contains(body, "resp_b") {
				t.Fatalf("resume=%d %s", status, body)
			}
			forkHeaders := http.Header{"Session_id": {child}, "X-Openai-Subagent": {"true"}, "X-Codex-Parent-Thread-Id": {parent}}
			status, body = h.httpRequest(t, "/v1/responses", continuation, forkHeaders)
			if status != 200 || !strings.Contains(body, "resp_b") {
				t.Fatalf("fork=%d %s", status, body)
			}
			install()
			status, body = h.httpRequest(t, "/v1/responses", continuation, http.Header{"Session_id": {child}})
			if status != 200 || !strings.Contains(body, "resp_b") {
				t.Fatalf("child resume=%d %s", status, body)
			}
			observations, _ := h.fixture.captured()
			if len(observations) != 4 {
				t.Fatalf("upstream requests=%d", len(observations))
			}
			for _, o := range observations {
				if o.account != "b" {
					t.Fatal("continuation reached a different account")
				}
			}
			status, _ = h.httpRequest(t, "/v1/responses", continuation, http.Header{"Session_id": {"unbound-thread"}})
			if status != 409 {
				t.Fatalf("unknown owner status=%d", status)
			}
			after, _ := h.fixture.captured()
			if len(after) != 4 {
				t.Fatal("unknown owner was sent upstream")
			}
		})
	}
}
