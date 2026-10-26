package helps

import "testing"

func TestCodexCapacityReplayGuard(t *testing.T) {
	for _, tc := range []struct {
		events []string
		safe   bool
	}{
		{[]string{`{"type":"response.created","response":{"output":[]}}`, `{"type":"response.failed"}`}, true},
		{[]string{`{"type":"response.web_search_call.in_progress"}`, `{"type":"response.failed"}`}, false},
		{[]string{`{"type":"response.output_item.added","item":{"type":"function_call"}}`, `{"type":"error"}`}, false},
		{[]string{`{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"error"}`}, false},
		{[]string{`{"type":"response.failed","response":{"output":[{"type":"web_search_call"}]}}`}, false},
		{[]string{`{"type":"unknown_event"}`}, false},
	} {
		var guard CodexCapacityReplayGuard
		for _, event := range tc.events {
			guard.Observe([]byte(event))
		}
		if guard.Safe() != tc.safe {
			t.Errorf("events=%v safe=%v", tc.events, guard.Safe())
		}
	}
}
