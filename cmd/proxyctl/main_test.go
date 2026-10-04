package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedeemRequiresExplicitAccountAndCredit(t *testing.T) {
	for _, args := range [][]string{{"resets", "redeem"}, {"resets", "redeem", "a"}} {
		var output bytes.Buffer
		if run(context.Background(), args, &output, func(string) string { return "" }, http.DefaultClient) == 0 || !strings.Contains(output.String(), `"code":"invalid_arguments"`) {
			t.Fatalf("unexpected result %s", output.String())
		}
	}
}

func TestSchedulePreservesExplicitInstantAndManagementCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/management/account-policy/resets/schedule" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer management-test" {
			t.Error("incorrect management request")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"at":"2026-10-25T01:30:00+02:00"`) || !strings.Contains(string(body), `"credit_id":"credit-1"`) {
			t.Errorf("wrong body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"schedule":{"id":"s"}}`)
	}))
	defer server.Close()
	env := func(name string) string {
		if name == "CLIPROXY_URL" {
			return server.URL
		}
		if name == "CLIPROXY_MANAGEMENT_KEY" {
			return "management-test"
		}
		return "inference-key-must-not-be-used"
	}
	var output bytes.Buffer
	if code := run(context.Background(), []string{"resets", "schedule", "a", "credit-1", "2026-10-25T01:30:00+02:00"}, &output, env, server.Client()); code != 0 {
		t.Fatalf("code=%d output=%s", code, output.String())
	}
}

func TestCLIRejectsRedirectWithoutSendingKey(t *testing.T) {
	var received bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	var output bytes.Buffer
	env := func(name string) string {
		if name == "CLIPROXY_URL" {
			return server.URL
		}
		return "management-test"
	}
	if run(context.Background(), []string{"accounts"}, &output, env, server.Client()) == 0 || received || strings.Contains(output.String(), "management-test") {
		t.Fatalf("unsafe redirect: %s", output.String())
	}
}

func TestCLIWithholdsUnexpectedServerContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "Bearer secret prompt body")
	}))
	defer server.Close()
	var output bytes.Buffer
	env := func(name string) string {
		if name == "CLIPROXY_URL" {
			return server.URL
		}
		return "management-test"
	}
	if run(context.Background(), []string{"accounts"}, &output, env, server.Client()) == 0 || strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), "server_error") {
		t.Fatal(output.String())
	}
}
