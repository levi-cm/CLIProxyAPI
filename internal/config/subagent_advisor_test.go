package config

import (
	"strings"
	"testing"
)

func TestSubagentAdvisorParsesAndValidates(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\nsubagent-advisor:\n  max-refresh-seconds: 1800\n  never-advise: [claude-fable-*, claude-mythos-*]\n  providers:\n    claude: {capacity-units: 15000}\n  models:\n    - {name: opus, match: [claude-opus-5-5], provider: claude, efforts: [xhigh], modes: [standard, fast], harnesses: {claude-code: opus}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolved := cfg.SubagentAdvisor.Resolve()
	if resolved.Providers["claude"].CapacityUnits != 15000 || len(resolved.Models) != 1 || resolved.Models[0].Efforts[0] != "xhigh" {
		t.Fatalf("resolved = %+v", resolved)
	}
	if resolved.Models[0].Harnesses["claude-code"] != "opus" || len(resolved.NeverAdvise) != 2 || resolved.MaxRefreshSeconds != 1800 {
		t.Fatalf("harnesses/never-advise = %+v %v", resolved.Models[0].Harnesses, resolved.NeverAdvise)
	}
	if _, err = ParseConfigBytes([]byte("port: 8317\nsubagent-advisor:\n  models:\n    - {name: bad, match: [x], efforts: [ultra]}\n")); err == nil || !strings.Contains(err.Error(), "subagent-advisor") {
		t.Fatalf("invalid effort accepted: %v", err)
	}
}

// The v8 layout comments out unknown root sections; subagent-advisor must survive.
func TestSubagentAdvisorSurvivesV8Layout(t *testing.T) {
	data := []byte("config-version: 8\nserver: {port: 8317}\nsubagent-advisor:\n  max-refresh-seconds: 900\n")
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SubagentAdvisor.MaxRefreshSeconds != 900 {
		t.Fatalf("v8 subagent-advisor lost: %+v", cfg.SubagentAdvisor)
	}
	normalized, _, err := NormalizeConfigLayout(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(normalized), "subagent-advisor:") || strings.Contains(string(normalized), "# subagent-advisor") {
		t.Fatalf("v8 migration commented out subagent-advisor:\n%s", normalized)
	}
}
