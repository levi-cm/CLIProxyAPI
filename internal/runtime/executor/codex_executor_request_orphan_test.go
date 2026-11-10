package executor

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// The Codex HTTP and WebSocket executors share translateCodexRequestPair, so
// orphan tool outputs sent over a Responses WebSocket are sanitized there.
func TestTranslateCodexRequestPairSanitizesOrphanToolOutputs(t *testing.T) {
	original := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call_output","id":"fco_1","name":"codex_apps_open_page","output":"ctx"}]}`)
	payload := []byte(`{"model":"gpt-5.4","input":[{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},{"type":"function_call_output","output":"paired"},{"type":"function_call_output","id":"fco_2","name":"codex_apps_open_page","output":"ctx"}]}`)

	originalTranslated, body := translateCodexRequestPair(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, "gpt-5.4", original, payload, true)

	if got := gjson.GetBytes(originalTranslated, "input.1.type").String(); got != "message" {
		t.Fatalf("original orphan output not sanitized: %s", originalTranslated)
	}
	if got := gjson.GetBytes(body, "input.1.call_id").String(); got != "call_1" {
		t.Fatalf("paired output call_id = %q, want call_1: %s", got, body)
	}
	if got := gjson.GetBytes(body, "input.2.type").String(); got != "message" {
		t.Fatalf("orphan output type = %q, want message: %s", got, body)
	}
	if got := gjson.GetBytes(body, "input.2.content.0.text").String(); got != "Tool output from codex_apps_open_page:\nctx" {
		t.Fatalf("orphan output text = %q", got)
	}
}
