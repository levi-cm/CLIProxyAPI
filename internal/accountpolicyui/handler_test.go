package accountpolicyui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerServesMaintainedAssetsWithoutCachingSecrets(t *testing.T) {
	for _, asset := range []struct{ path, contentType string }{{"/account-policy.html", "text/html; charset=utf-8"}, {"/account-policy.js", "text/javascript; charset=utf-8"}, {"/account-policy.css", "text/css; charset=utf-8"}} {
		r := httptest.NewRecorder()
		Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, asset.path, nil))
		if r.Code != http.StatusOK || r.Header().Get("Content-Type") != asset.contentType || r.Body.Len() == 0 {
			t.Fatalf("%s: status=%d content=%s", asset.path, r.Code, r.Header().Get("Content-Type"))
		}
		if r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s lacks security headers", asset.path)
		}
	}
}

func TestHandlerRejectsUnregisteredAssetsAndWrites(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/management.html", 404}, {"GET", "/../account-policy.html", 404}, {"POST", "/account-policy.html", 405}} {
		r := httptest.NewRecorder()
		Handler().ServeHTTP(r, httptest.NewRequest(tc.method, tc.path, nil))
		if r.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, r.Code, tc.status)
		}
	}
}
