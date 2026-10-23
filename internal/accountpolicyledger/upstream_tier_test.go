package accountpolicyledger

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"testing"
)

func TestRawHistoryPreservesRequestedEffectiveAndResponseTiers(t *testing.T) {
	item := Allowlisted(usage.Record{ServiceTier: "auto", EffectiveServiceTier: "priority", ResponseServiceTier: "default"})
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["requested_service_tier"] != "auto" || fields["effective_service_tier"] != "priority" || fields["response_service_tier"] != "default" {
		t.Fatalf("lost tier identities: %s", data)
	}
}
