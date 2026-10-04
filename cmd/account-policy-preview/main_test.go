package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviewRejectsPublicListener(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "8.8.8.8:8080", "example.com:8080"} {
		if err := validateListen(addr); err == nil {
			t.Fatalf("accepted public listener %q", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "100.82.251.30:8080"} {
		if err := validateListen(addr); err != nil {
			t.Fatalf("safe listener %q: %v", addr, err)
		}
	}
}

func TestPreviewRequiresFixtureKeyAndRejectsCrossAccountCredit(t *testing.T) {
	h := newFixture().handler()
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/v8/management/account-policy/accounts", nil))
	if r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", r.Code)
	}
	request := httptest.NewRequest("POST", "/v8/management/account-policy/resets/redeem", strings.NewReader(`{"credential_id":"account-a","credit_id":"october-05"}`))
	request.Header.Set("Authorization", "Bearer fixture-key")
	r = httptest.NewRecorder()
	h.ServeHTTP(r, request)
	if r.Code != http.StatusConflict {
		t.Fatalf("cross-account credit status=%d body=%s", r.Code, r.Body.String())
	}
}
