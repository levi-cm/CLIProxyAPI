package accountpolicyclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The advertised tool and protocol must work over JSON, without exposing writes.
func TestMCPReadOnlyUsageProtocol(t *testing.T) {
	reads := 0
	for _, test := range []struct {
		body     string
		status   int
		contains string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"0.160.0"}}}`, 200, `"protocolVersion":"2025-06-18"`},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202, ``},
		{`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200, `"readOnlyHint":true`},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"proxy_account_usage","arguments":{"session_id":"thread-one"}}}`, 200, `"structuredContent"`},
		{`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"reset","arguments":{"session_id":"thread-one"}}}`, 200, `"code":-32602`},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"proxy_account_usage","arguments":{}}}`, 200, `"code":-32602`},
		{`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"proxy_account_usage","arguments":{"session_id":"thread-one","reset":true}}}`, 200, `"code":-32602`},
		{`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"proxy_account_usage","arguments":{"session_id":"bad\nthread"}}}`, 200, `"code":-32602`},
		{`{"jsonrpc":"2.0","id":8,"method":"reset"}`, 200, `"code":-32601`},
		{`{"jsonrpc":"2.0","id":9,"method":"ping"}`, 200, `"result":{}`},
		{`{"jsonrpc":"2.0","id":10,"method":"tools/list"} {}`, 400, `"code":-32700`},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://proxy.local/v1/account-policy/mcp", strings.NewReader(test.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ServeMCP(w, r, func(sessionID string) (Report, error) {
			reads++
			if sessionID != "thread-one" {
				t.Fatalf("wrong session: %q", sessionID)
			}
			return Report{SchemaVersion: 1, SessionID: sessionID, State: "idle", ActiveAccounts: []Account{}}, nil
		})
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.contains) {
			t.Fatalf("%s: status=%d body=%s", test.body, w.Code, w.Body.String())
		}
		if w.Code == 200 {
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["jsonrpc"] != "2.0" || body["id"] == nil {
				t.Fatalf("invalid JSON-RPC envelope: %s", w.Body.String())
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("quota response allows caching")
		}
	}
	if reads != 1 {
		t.Fatalf("non-tool requests read account data: %d reads", reads)
	}
}

func TestMCPRejectsCrossOriginRequests(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://proxy.local/v1/account-policy/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://untrusted.example")
	w := httptest.NewRecorder()
	ServeMCP(w, r, nil)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-origin MCP request was accepted")
	}
}
