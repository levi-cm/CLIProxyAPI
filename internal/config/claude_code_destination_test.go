package config

import "testing"

func TestParseConfigBytesClaudeCodeRestrictAnthropicModels(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{name: "defaults to disabled", yaml: "port: 8317\n", want: false},
		{name: "legacy layout", yaml: "claude-code:\n  restrict-anthropic-models: true\n", want: true},
		{name: "v8 layout", yaml: "config-version: 8\nupstream:\n  claude:\n    restrict-anthropic-models: true\n", want: true},
		{name: "v8 explicit false", yaml: "config-version: 8\nupstream:\n  claude:\n    restrict-anthropic-models: false\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.ClaudeCode.RestrictAnthropicModels; got != tt.want {
				t.Fatalf("RestrictAnthropicModels = %t, want %t", got, tt.want)
			}
		})
	}
}
