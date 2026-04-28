package llmagent

import "testing"

func TestBaseAndModel(t *testing.T) {
	tests := []struct {
		in          string
		base, model string
		local       bool
	}{
		{"claude", "claude", "", false},
		{"gemini", "gemini", "", false},
		{"gemini:gemini-2.5-pro", "gemini", "gemini-2.5-pro", false},
		{"opencode", "opencode", "", true},
		{"opencode:kimi-k2", "opencode", "kimi-k2", true},
		{"crush:some-model", "crush", "some-model", true},
		{"codex-oss", "codex-oss", "", true},
		{"codex", "codex", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := Base(tt.in); got != tt.base {
				t.Errorf("Base(%q) = %q, want %q", tt.in, got, tt.base)
			}
			if got := Model(tt.in); got != tt.model {
				t.Errorf("Model(%q) = %q, want %q", tt.in, got, tt.model)
			}
			if got := IsLocal(tt.in); got != tt.local {
				t.Errorf("IsLocal(%q) = %v, want %v", tt.in, got, tt.local)
			}
		})
	}
}
