// proxyctl operates the authenticated account policy API. Supply the tailnet
// endpoint in CLIPROXY_URL and the separate management key in
// CLIPROXY_MANAGEMENT_KEY or CLIPROXY_MANAGEMENT_KEY_FILE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Getenv, http.DefaultClient))
}

func failure(out io.Writer, code, message string) int {
	_ = json.NewEncoder(out).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return 1
}

func run(ctx context.Context, args []string, out io.Writer, getenv func(string) string, client *http.Client) int {
	method, path := http.MethodGet, ""
	var payload any
	if len(args) == 1 {
		switch args[0] {
		case "accounts":
			path = "/accounts"
		case "explain":
			path = "/decisions"
		case "refresh":
			method, path, payload = http.MethodPost, "/refresh", map[string]string{"credential_id": ""}
		}
	}
	if len(args) == 2 && args[0] == "refresh" {
		method, path, payload = http.MethodPost, "/refresh", map[string]string{"credential_id": args[1]}
	}
	if len(args) >= 2 && args[0] == "resets" {
		switch args[1] {
		case "list":
			if len(args) == 2 {
				path = "/resets"
			}
		case "redeem":
			if len(args) == 4 && strings.TrimSpace(args[2]) != "" && strings.TrimSpace(args[3]) != "" {
				method, path, payload = http.MethodPost, "/resets/redeem", map[string]string{"credential_id": args[2], "credit_id": args[3]}
			}
		case "schedule":
			if len(args) == 5 && strings.TrimSpace(args[2]) != "" && strings.TrimSpace(args[3]) != "" {
				if _, err := time.Parse(time.RFC3339Nano, args[4]); err != nil {
					return failure(out, "invalid_arguments", "Schedule time must be RFC3339 with an explicit offset.")
				}
				method, path, payload = http.MethodPost, "/resets/schedule", map[string]string{"credential_id": args[2], "credit_id": args[3], "at": args[4]}
			}
		case "cancel":
			if len(args) == 3 && strings.TrimSpace(args[2]) != "" {
				method, path = http.MethodDelete, "/resets/schedule/"+url.PathEscape(args[2])
			}
		}
	}
	if path == "" {
		return failure(out, "invalid_arguments", "Usage: proxyctl accounts|explain|refresh [credential_id]|resets list|resets redeem <credential_id> <credit_id>|resets schedule <credential_id> <credit_id> <RFC3339>|resets cancel <schedule_id>")
	}
	base, err := url.Parse(strings.TrimSpace(getenv("CLIPROXY_URL")))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return failure(out, "invalid_configuration", "Set CLIPROXY_URL to the private tailnet server origin, without a path or credentials.")
	}
	key := strings.TrimSpace(getenv("CLIPROXY_MANAGEMENT_KEY"))
	if key == "" {
		if file := strings.TrimSpace(getenv("CLIPROXY_MANAGEMENT_KEY_FILE")); file != "" {
			content, errRead := os.ReadFile(file)
			if errRead != nil {
				return failure(out, "invalid_configuration", "Cannot read the management key file.")
			}
			key = strings.TrimSpace(string(content))
		}
	}
	if key == "" {
		return failure(out, "invalid_configuration", "Set CLIPROXY_MANAGEMENT_KEY or CLIPROXY_MANAGEMENT_KEY_FILE to the separate management credential.")
	}
	var body io.Reader
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		body = bytes.NewReader(encoded)
	}
	endpoint := strings.TrimSuffix(base.String(), "/") + "/v8/management/account-policy" + path
	request, errRequest := http.NewRequestWithContext(ctx, method, endpoint, body)
	if errRequest != nil {
		return failure(out, "invalid_configuration", "Cannot construct the management request.")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// Never follow a redirect with an operator credential or replay a write.
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, errResponse := safeClient.Do(request)
	if errResponse != nil {
		return failure(out, "connection_error", "Cannot reach the private proxy endpoint; check tailnet connectivity and listener configuration.")
	}
	defer func() { _ = response.Body.Close() }()
	content, errRead := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if errRead != nil || len(content) > 8<<20 {
		return failure(out, "invalid_response", "Cannot read a bounded management response.")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var result struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(content, &result) == nil && safeErrorCode(result.Error.Code) {
			return failure(out, result.Error.Code, "Management request rejected.")
		}
		if response.StatusCode == 401 || response.StatusCode == 403 {
			return failure(out, "unauthorized", "Management access denied.")
		}
		return failure(out, "server_error", "Management request failed.")
	}
	if !json.Valid(content) {
		return failure(out, "invalid_response", "The management endpoint returned invalid JSON.")
	}
	if _, errWrite := out.Write(append(bytes.TrimSpace(content), '\n')); errWrite != nil {
		return 1
	}
	return 0
}

func safeErrorCode(code string) bool {
	switch code {
	case "unavailable", "invalid_request", "invalid_settings", "unknown_account", "unknown_credit", "unknown_schedule", "conflict", "persistence", "read_only", "disabled", "expired", "stale_evidence", "identity_mismatch", "provider_error", "invalid_schedule", "unauthorized", "forbidden":
		return true
	}
	return false
}
