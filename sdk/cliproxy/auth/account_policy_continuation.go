package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// Session identifiers alone do not imply stateful input. Inspect protocol fields,
// never arbitrary tool schemas, tool outputs, metadata, or JSON inside text.
func policyRequestHasLocalState(opts cliproxyexecutor.Options) bool {
	return policyContinuationCategory(opts) != ""
}

func policyContinuationCategory(opts cliproxyexecutor.Options) string {
	for _, key := range []string{"turn_state", "previous_response_id", "encrypted_reasoning", "encrypted_content"} {
		if stateValuePresent(opts.Metadata[key]) {
			return "execution_metadata." + key
		}
	}
	var root map[string]any
	if json.Unmarshal(opts.OriginalRequest, &root) != nil {
		return ""
	}
	if category := responseContinuationCategory(root); category != "" {
		return category
	}
	// Some Responses transports wrap create parameters in response. This is a
	// single protocol envelope, not a recursive walk through application data.
	if kind, _ := root["type"].(string); kind == "response.create" || kind == "response.append" {
		if response, ok := root["response"].(map[string]any); ok {
			return responseContinuationCategory(response)
		}
	}
	return ""
}

func stateValuePresent(v any) bool { return v != nil && v != "" }

func responseContinuationCategory(root map[string]any) string {
	if reasoning, ok := root["reasoning"].(map[string]any); ok {
		for _, key := range []string{"encrypted_content", "encrypted_reasoning"} {
			if stateValuePresent(reasoning[key]) {
				return "reasoning." + key
			}
		}
	}

	for _, key := range []string{"previous_response_id", "conversation_id", "conversation", "turn_state", "encrypted_reasoning", "encrypted_content"} {
		if stateValuePresent(root[key]) {
			return key
		}
	}
	for _, key := range []string{"input", "messages"} {
		items, _ := root[key].([]any)
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if category := continuationItemCategory(item); category != "" {
				return category
			}
			kind, _ := item["type"].(string)
			role, _ := item["role"].(string)
			if kind == "message" || kind == "" && (role == "assistant" || role == "user") {
				content, _ := item["content"].([]any)
				for _, part := range content {
					if obj, ok := part.(map[string]any); ok {
						if category := continuationItemCategory(obj); category != "" {
							return category
						}
					}
				}
			}
		}
	}
	return ""
}

func continuationItemCategory(item map[string]any) string {
	kind, _ := item["type"].(string)
	switch kind {
	case "item_reference", "compaction":
		return kind
	case "reasoning":
		for _, key := range []string{"encrypted_content", "encrypted_reasoning"} {
			if stateValuePresent(item[key]) {
				return "reasoning." + key
			}
		}
	}
	return ""
}

func policyDiagnosticHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func logPolicyRejection(ctx context.Context, model string, opts cliproxyexecutor.Options, err error) {
	var rejection *Error
	if !errors.As(err, &rejection) || !strings.HasPrefix(rejection.Code, "account_policy_") && rejection.Code != "unsafe_affinity_unavailable" {
		return
	}
	session, _ := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	lookup, _ := opts.Metadata["account_policy_binding_lookup"].(string)
	switch lookup {
	case "hit", "miss", "error", "unavailable":
	default:
		lookup = "not_attempted"
	}
	trace := logging.GetRequestID(ctx)
	if len(trace) > 128 {
		trace = policyDiagnosticHash(trace)
	}
	// All fields have fixed categories or bounded identities. No request body,
	// continuation contents or raw caller/session identifiers reach this log.
	canonical := canonicalModelKey(model)
	if len(canonical) > 128 {
		canonical = policyDiagnosticHash(canonical)
	}
	log.WithFields(log.Fields{"component": "account-policy-rejection", "trace_id": trace, "classification": rejection.Code, "session_hash": policyDiagnosticHash(policyClientScope(opts) + "\x00" + session), "model": canonical, "binding_lookup": lookup, "continuation": policyContinuationCategory(opts)}).Warn("conversation ownership rejected")
}
