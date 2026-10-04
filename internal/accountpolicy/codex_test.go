package accountpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Sanitized fixtures follow openai/codex afb436df8b70bb5bc57b86d9a3e829968988cd21
// backend-client/src/client/rate_limit_resets_tests.rs and generated quota models.
func codexFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/codex_" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func codexIdentity() Identity {
	return Identity{CredentialID: "credential-a", AccountID: "fixture-account-a", WorkspaceID: "fixture-account-a", Provider: "codex", Generation: 1}
}
func codexTestClient(server *httptest.Server) *CodexClient {
	return &CodexClient{BaseURL: server.URL, HTTPClient: server.Client(), Credential: func(_ context.Context, id string) (CodexCredential, error) {
		if id != "credential-a" {
			return CodexCredential{}, fmt.Errorf("unknown credential")
		}
		return CodexCredential{AccessToken: "fixture-secret", AccountID: "fixture-account-a", WorkspaceID: "fixture-account-a"}, nil
	}}
}
func TestCodexDiscoverPinnedProtocol(t *testing.T) {
	usage, credits := codexFixture(t, "usage"), codexFixture(t, "credits")
	for _, style := range []string{"chatgpt", "codex"} {
		t.Run(style, func(t *testing.T) {
			prefix := "/backend-api/wham/"
			if style == "codex" {
				prefix = "/api/codex/"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("ChatGPT-Account-Id") != "fixture-account-a" {
					t.Errorf("incorrect authenticated read")
				}
				switch r.URL.Path {
				case prefix + "usage":
					fmt.Fprint(w, usage)
				case prefix + "rate-limit-reset-credits":
					fmt.Fprint(w, credits)
				default:
					t.Errorf("wrong path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := codexTestClient(server)
			c.PathStyle = style
			s, err := c.Discover(context.Background(), codexIdentity())
			if err != nil {
				t.Fatal(err)
			}
			if s.Identity != codexIdentity() || !s.Eligible || s.WritesDisabled || !s.InventoryComplete || s.AvailableCredits != 3 {
				t.Fatalf("bad snapshot %#v", s)
			}
			if len(s.Buckets) != 3 || s.Buckets[0].DurationSeconds != 18000 || s.Buckets[1].DurationSeconds != 604800 || s.Buckets[2].Scope != "codex_model_fixture" || s.Buckets[2].Model != "fixture-model" || s.Buckets[2].Allowed == nil || *s.Buckets[2].Allowed {
				t.Fatalf("bad buckets %#v", s.Buckets)
			}
			if len(s.Credits) != 3 || s.Credits[0].Type != "codex_rate_limits" || !s.Credits[0].DetailsKnown || s.Credits[0].Scopes[0] != "ordinary" || s.Credits[2].ExpiresAt.Format(time.RFC3339) != "2026-10-29T18:48:00Z" {
				t.Fatalf("bad credits %#v", s.Credits)
			}
		})
	}
}
func TestCodexCreditMissingExpiryIsNotNonexpiring(t *testing.T) {
	for _, tc := range []struct {
		name, expiry string
		known        bool
	}{{"absent", "", false}, {"null", `,"expires_at":null`, true}} {
		t.Run(tc.name, func(t *testing.T) {
			usage := codexFixture(t, "usage")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/usage") {
					fmt.Fprint(w, usage)
					return
				}
				fmt.Fprintf(w, `{"available_count":3,"credits":[{"id":"fixture-credit","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-03T00:00:00Z"%s}]}`, tc.expiry)
			}))
			defer server.Close()
			s, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Credits) != 1 || s.Credits[0].DetailsKnown != tc.known || s.Credits[0].ExpiresAt != nil || s.InventoryComplete {
				t.Fatalf("details fabricated %#v", s)
			}
		})
	}
}
func TestCodexConsumeSelectedIDAndOutcomes(t *testing.T) {
	for _, code := range []string{"reset", "nothing_to_reset", "no_credit", "already_redeemed"} {
		t.Run(code, func(t *testing.T) {
			usage, credits := codexFixture(t, "usage"), codexFixture(t, "credits")
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/usage"):
					fmt.Fprint(w, usage)
				case r.Method == "GET":
					fmt.Fprint(w, credits)
				case r.Method == "POST":
					posts.Add(1)
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["redeem_request_id"] != "8ca2f7b0-7c38-4e25-a027-4a4556cb17f4" || body["credit_id"] != "fixture-credit-second" || len(body) != 2 {
						t.Errorf("wrong selected request %#v", body)
					}
					fmt.Fprintf(w, `{"code":%q,"windows_reset":2}`, code)
				}
			}))
			defer server.Close()
			c := codexTestClient(server)
			for range 2 {
				res, err := c.Consume(context.Background(), codexIdentity(), "8ca2f7b0-7c38-4e25-a027-4a4556cb17f4", "fixture-credit-second")
				if err != nil || res.Code != code || res.WindowsReset != 2 {
					t.Fatalf("result %#v %v", res, err)
				}
			}
			if posts.Load() != 2 {
				t.Fatalf("posts %d", posts.Load())
			}
		})
	}
}
func TestCodexBlocksIdentityAndSchemaWrites(t *testing.T) {
	for _, tc := range []struct {
		name, usage, credits string
		alter                func(*CodexClient, *Identity)
	}{
		{name: "mismatched-account", usage: strings.ReplaceAll(codexFixture(t, "usage"), "fixture-account-a", "other-account")},
		{name: "missing-account", usage: strings.ReplaceAll(codexFixture(t, "usage"), `"account_id": "fixture-account-a",`, "")},
		{name: "malformed-usage", usage: `{"account_id":"fixture-account-a","rate_limit":{}}`},
		{name: "malformed-inventory", credits: `{"credits":[],"available_count":"three"}`},
		{name: "foreign-workspace", alter: func(_ *CodexClient, id *Identity) { id.WorkspaceID = "other-workspace" }},
		{name: "foreign-credential", alter: func(c *CodexClient, _ *Identity) {
			c.Credential = func(context.Context, string) (CodexCredential, error) {
				return CodexCredential{AccessToken: "secret", AccountID: "other-account"}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, credits := tc.usage, tc.credits
			if usage == "" {
				usage = codexFixture(t, "usage")
			}
			if credits == "" {
				credits = codexFixture(t, "credits")
			}
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
					fmt.Fprint(w, `{"code":"reset"}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/usage") {
					fmt.Fprint(w, usage)
				} else {
					fmt.Fprint(w, credits)
				}
			}))
			defer server.Close()
			c, id := codexTestClient(server), codexIdentity()
			if tc.alter != nil {
				tc.alter(c, &id)
			}
			_, err := c.Consume(context.Background(), id, "8ca2f7b0-7c38-4e25-a027-4a4556cb17f4", "fixture-credit-first")
			if err == nil || posts.Load() != 0 {
				t.Fatalf("unsafe write %v posts=%d", err, posts.Load())
			}
		})
	}
}
func TestCodexNeverFollowsCredentialRedirect(t *testing.T) {
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("redirect leak %v %d", err, leaked.Load())
	}
}
func TestCodexRefreshesOnceAndSanitizesErrors(t *testing.T) {
	var reads, refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.WriteHeader(401)
		fmt.Fprint(w, "fixture-secret PRIVATE-PROMPT")
	}))
	defer server.Close()
	c := codexTestClient(server)
	c.RefreshCredential = func(context.Context, string) error { refreshes.Add(1); return nil }
	_, err := c.Discover(context.Background(), codexIdentity())
	if err == nil || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "PRIVATE-PROMPT") || reads.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("refresh/error %v %d %d", err, reads.Load(), refreshes.Load())
	}
}

func TestCodexLostResponseRetryCanReconcileConsumedCredit(t *testing.T) {
	usage := codexFixture(t, "usage")
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, usage)
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"available_count":0,"credits":[]}`)
			return
		}
		posts.Add(1)
		fmt.Fprint(w, `{"code":"already_redeemed"}`)
	}))
	defer server.Close()
	res, err := codexTestClient(server).Consume(context.Background(), codexIdentity(), "8ca2f7b0-7c38-4e25-a027-4a4556cb17f4", "fixture-credit-first")
	if err != nil || res.Code != "already_redeemed" || posts.Load() != 1 {
		t.Fatalf("lost response cannot reconcile: %#v %v %d", res, err, posts.Load())
	}
}

func TestCodexSchemaFailureDisablesWritesAndRateLimitAdvice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, codexFixture(t, "usage"))
			return
		}
		fmt.Fprint(w, `{"credits":[]}`)
	}))
	defer server.Close()
	snapshot, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err == nil || !snapshot.WritesDisabled {
		t.Fatalf("schema change permits writes: %#v %v", snapshot, err)
	}
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "300"); w.WriteHeader(429) }))
	defer limited.Close()
	_, err = codexTestClient(limited).Discover(context.Background(), codexIdentity())
	var advice interface{ RetryDelay() time.Duration }
	if !errors.As(err, &advice) || advice.RetryDelay() != 300*time.Second {
		t.Fatalf("rate limit advice lost %v", err)
	}
}

func TestCodexReusesProviderConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, codexFixture(t, "usage"))
		} else {
			fmt.Fprint(w, codexFixture(t, "credits"))
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	c := codexTestClient(server)
	for range 2 {
		if _, err := c.Discover(context.Background(), codexIdentity()); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connection reuse lost: %d", connections.Load())
	}
}

func TestCodexAccountScopedTransportAndCredentials(t *testing.T) {
	usage, credits := codexFixture(t, "usage"), codexFixture(t, "credits")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("ChatGPT-Account-Id")
		if r.Header.Get("Authorization") != "Bearer token-"+account {
			t.Error("account credentials crossed")
		}
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, strings.ReplaceAll(usage, "fixture-account-a", account))
		} else {
			fmt.Fprint(w, credits)
		}
	}))
	defer server.Close()
	var selected atomic.Int32
	c := &CodexClient{BaseURL: server.URL, HTTPClientForCredential: func(id string) *http.Client {
		if id != "a" && id != "b" {
			t.Error("wrong transport credential")
		}
		selected.Add(1)
		return server.Client()
	}, Credential: func(_ context.Context, id string) (CodexCredential, error) {
		return CodexCredential{AccessToken: "token-" + id, AccountID: id, WorkspaceID: id}, nil
	}}
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) {
			snapshot, err := c.Discover(context.Background(), Identity{CredentialID: id, AccountID: id, WorkspaceID: id, Provider: "codex"})
			if err == nil && snapshot.Identity.AccountID != id {
				err = fmt.Errorf("crossed identity")
			}
			errs <- err
		}(id)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if selected.Load() != 4 {
		t.Fatalf("account transport bypassed %d", selected.Load())
	}
}

func TestCodexUnsupportedInventoryRetainsUsageAndSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, codexFixture(t, "usage"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	s, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err != nil || s.AvailableCredits != 3 || len(s.Credits) != 0 || s.InventoryComplete || !s.WritesDisabled || len(s.Buckets) != 3 {
		t.Fatalf("unsupported inventory fabricated %#v %v", s, err)
	}
}

func TestCodexUnknownCreditTypesNeverConsume(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, codexFixture(t, "usage"))
			return
		}
		fmt.Fprint(w, strings.ReplaceAll(codexFixture(t, "credits"), "codex_rate_limits", "unknown_offer"))
	}))
	defer server.Close()
	c := codexTestClient(server)
	s, err := c.Discover(context.Background(), codexIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if s.Credits[0].Type != "unknown_offer" || len(s.Credits[0].Scopes) != 0 {
		t.Fatalf("scope fabricated %#v", s.Credits[0])
	}
	_, err = c.Consume(context.Background(), codexIdentity(), "8ca2f7b0-7c38-4e25-a027-4a4556cb17f4", "fixture-credit-first")
	if err == nil || posts.Load() != 0 {
		t.Fatalf("unknown credit consumed %v", err)
	}
}

func TestCodexAdditionalCodeReviewScopeDoesNotBlockOrdinary(t *testing.T) {
	usage := strings.ReplaceAll(codexFixture(t, "usage"), "codex_model_fixture", "code_review")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, usage)
		} else {
			fmt.Fprint(w, codexFixture(t, "credits"))
		}
	}))
	defer server.Close()
	s, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err != nil || !s.Eligible || s.Buckets[2].Scope != "code_review" || s.Buckets[2].DurationSeconds != 300 {
		t.Fatalf("review quota conflated %#v %v", s, err)
	}
}

func TestCodexForbiddenDoesNotRefreshAndConsumeSchemaIsClosed(t *testing.T) {
	var refreshed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, "PRIVATE-PROMPT fixture-secret")
	}))
	defer server.Close()
	c := codexTestClient(server)
	c.RefreshCredential = func(context.Context, string) error { refreshed.Add(1); return nil }
	_, err := c.Discover(context.Background(), codexIdentity())
	if err == nil || refreshed.Load() != 0 || strings.Contains(err.Error(), "PRIVATE-PROMPT") {
		t.Fatalf("incorrect forbidden handling %v", err)
	}
	for _, body := range []string{`{"code":"future_code"}`, `{"code":"reset","windows_reset":-1}`, `{"code":"reset","windows_reset":"2"}`, `{"success":true}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					fmt.Fprint(w, body)
				} else if strings.HasSuffix(r.URL.Path, "/usage") {
					fmt.Fprint(w, codexFixture(t, "usage"))
				} else {
					fmt.Fprint(w, codexFixture(t, "credits"))
				}
			}))
			defer s.Close()
			_, err := codexTestClient(s).Consume(context.Background(), codexIdentity(), "8ca2f7b0-7c38-4e25-a027-4a4556cb17f4", "fixture-credit-first")
			if err == nil {
				t.Fatal("schema accepted")
			}
		})
	}
}

func TestCodexDoesNotAcceptUnroutableBackendConfiguration(t *testing.T) {
	for _, base := range []string{"http://public.example", "https://user:secret@chatgpt.com", "https://chatgpt.com?token=secret", "https://chatgpt.com/unverified"} {
		c := &CodexClient{BaseURL: base}
		_, err := c.Discover(context.Background(), codexIdentity())
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe configuration %v", err)
		}
	}
}

func TestCodexMissingOrdinaryWindowsCannotAuthorizeWrite(t *testing.T) {
	usage := `{"plan_type":"plus","account_id":"fixture-account-a","rate_limit":{"allowed":true,"limit_reached":false},"additional_rate_limits":[{"limit_name":"Model","metered_feature":"model","normal_model_slug":"fixture-model","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":5,"limit_window_seconds":300,"reset_at":2000000000}}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			fmt.Fprint(w, usage)
		} else {
			fmt.Fprint(w, codexFixture(t, "credits"))
		}
	}))
	defer server.Close()
	s, err := codexTestClient(server).Discover(context.Background(), codexIdentity())
	if err != nil || !s.WritesDisabled {
		t.Fatalf("model allowance authorized ordinary reset %#v %v", s, err)
	}
}
