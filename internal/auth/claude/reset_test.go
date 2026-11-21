package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type resetRoundTripper struct {
	status int
	body   string
	err    error
	seen   *http.Request
	sent   []byte
}

func (rt *resetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.seen = req
	if req.Body != nil {
		rt.sent, _ = io.ReadAll(req.Body)
	}
	if rt.err != nil {
		return nil, rt.err
	}
	return &http.Response{StatusCode: rt.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(rt.body)), Request: req}, nil
}

const testOrg = "0d2c4b36-1b55-4a5e-8f3a-1f6a2b9c7d10"

func TestClaimRateLimitResetRequestAndResults(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		err     error
		want    string
		unknown bool
	}{
		{200, `{"result":"reset"}`, nil, ResetResultReset, false},
		{200, `{"result":"already_used"}`, nil, ResetResultAlreadyUsed, false},
		{200, `{"result":"not_limited"}`, nil, ResetResultNotLimited, false},
		{429, `{}`, nil, ResetResultRateLimited, false},
		{403, `{}`, nil, ResetResultAuthError, false},
		{200, `{"result":"surprise"}`, nil, "", true},
		{502, `bad gateway`, nil, "", true},
		{0, ``, errors.New("connection reset"), "", true},
	}
	for _, c := range cases {
		rt := &resetRoundTripper{status: c.status, body: c.body, err: c.err}
		auth := &ClaudeAuth{httpClient: &http.Client{Transport: rt}}
		got, err := auth.ClaimRateLimitReset(context.Background(), "tok", testOrg, "full-reset-2611", "req-1")
		if c.unknown {
			if !errors.Is(err, ErrResetOutcomeUnknown) {
				t.Fatalf("status %d: err = %v, want unknown outcome", c.status, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("status %d: got %q, %v; want %q", c.status, got, err, c.want)
		}
		req := rt.seen
		if req.Method != http.MethodPost || req.URL.String() != "https://api.anthropic.com/api/organizations/"+testOrg+"/reset_rate_limits" {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("User-Agent") != resetClientUserAgent || req.Header.Get("anthropic-beta") != resetClientBeta || req.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("headers = %v", req.Header)
		}
		var payload map[string]string
		if errDecode := json.Unmarshal(rt.sent, &payload); errDecode != nil || payload["program"] != "cedar_ember" || payload["grant_id"] != "full-reset-2611" || payload["request_id"] != "req-1" {
			t.Fatalf("payload = %s", rt.sent)
		}
	}
}

func TestClaimRateLimitResetRejectsInvalidIdentifiers(t *testing.T) {
	rt := &resetRoundTripper{status: 200, body: `{"result":"reset"}`}
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: rt}}
	for _, args := range [][3]string{{"not-a-uuid", "full", "req"}, {testOrg, "Bad Grant", "req"}, {testOrg, "full", "bad request id!"}} {
		if _, err := auth.ClaimRateLimitReset(context.Background(), "tok", args[0], args[1], args[2]); err == nil || errors.Is(err, ErrResetOutcomeUnknown) {
			t.Fatalf("%v: err = %v, want validation error", args, err)
		}
	}
	if rt.seen != nil {
		t.Fatal("invalid claim reached the network")
	}
}

func TestFetchOAuthUsageWithResetsRequiresInventory(t *testing.T) {
	rt := &resetRoundTripper{status: 200, body: `{"seven_day":{"utilization":7,"resets_at":"2026-11-27T03:00:00Z"}}`}
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: rt}}
	if _, err := auth.FetchOAuthUsageWithResets(context.Background(), "tok"); err == nil {
		t.Fatal("usage without reset inventory accepted")
	}
	if rt.seen.URL.String() != UsageWithResetsURL || rt.seen.Header.Get("User-Agent") != resetClientUserAgent {
		t.Fatalf("request = %s %v", rt.seen.URL, rt.seen.Header)
	}
}
