package accountpolicyclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const ToolName = "proxy_account_usage"

// ValidSessionID accepts explicit Codex thread IDs without control characters.
// It does not derive identities from prompts, IP addresses, or global activity.
func ValidSessionID(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// ServeMCP implements the stateless Streamable HTTP request/JSON-response subset.
// Authentication and caller scoping belong to the embedding HTTP route. No MCP
// sessions, background streams, provider calls, or mutating tools are exposed.
func ServeMCP(w http.ResponseWriter, r *http.Request, source func(string) (Report, error)) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request rpcRequest
	var raw json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	failure := func(status, code int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpcID(request.ID), "error": map[string]any{"code": code, "message": message}})
	}
	if err := decoder.Decode(&raw); err != nil || json.Unmarshal(raw, &request) != nil {
		failure(400, -32700, "Invalid JSON request.")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		failure(400, -32700, "Expected one JSON request.")
		return
	}
	if request.JSONRPC != "2.0" || request.Method == "" || !validRPCID(request.ID) {
		failure(400, -32600, "Invalid JSON-RPC request.")
		return
	}
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	respond := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpcID(request.ID), "result": result})
	}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(request.Params, &params) != nil {
			failure(200, -32602, "Invalid initialization parameters.")
			return
		}
		version := params.ProtocolVersion
		if version != "2024-11-05" && version != "2025-03-26" && version != "2025-06-18" && version != "2025-11-25" {
			version = "2025-06-18"
		}
		respond(map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "cliproxy-account-usage", "version": "1.0.0"}, "instructions": "Use proxy_account_usage for the exact current Codex thread. Obtain its session_id from CODEX_THREAD_ID in the current execution environment, or the Session value in /status. Never guess a session ID. Active accounts are live attempts; when idle, report the last successful account as last used, not currently serving. Weekly usage is provider-reported; unavailable values are not zero."})
	case "ping":
		respond(map[string]any{})
	case "tools/list":
		respond(map[string]any{"tools": []any{map[string]any{
			"name": ToolName, "title": "Proxy account and weekly usage",
			"description": "Read the account(s) handling this exact Codex session, the last successful account, weekly used and remaining percentages, and the seven-day window start and refresh/end time. Requires the actual session_id from CODEX_THREAD_ID or /status; never use a global or guessed account. No provider calls or resets.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 256, "description": "The exact Codex thread UUID (Session in /status; CODEX_THREAD_ID)."}}, "required": []string{"session_id"}, "additionalProperties": false},
			"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		}}})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		var args struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(request.Params, &params) != nil || params.Name != ToolName || !strictArguments(params.Arguments, &args) || !ValidSessionID(args.SessionID) {
			failure(200, -32602, "Expected proxy_account_usage with only a valid session_id.")
			return
		}
		if source == nil {
			respond(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "Session usage is unavailable."}}})
			return
		}
		report, err := source(args.SessionID)
		if err != nil {
			respond(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "Session usage is unavailable; check proxy observation configuration and the exact session ID."}}})
			return
		}
		encoded, errEncode := json.Marshal(report)
		if errEncode != nil {
			failure(200, -32603, "Usage report could not be encoded.")
			return
		}
		respond(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}, "structuredContent": report, "isError": false})
	default:
		failure(200, -32601, "Only read-only usage methods are available.")
	}
}

func strictArguments(raw json.RawMessage, out any) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil
}

func rpcID(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func validRPCID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return false
	}
	switch id.(type) {
	case string, float64:
		return true
	default:
		return false
	}
}
