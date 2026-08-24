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
		{"opencode:opencode/deepseek-v4-flash", "opencode", "opencode/deepseek-v4-flash", true},
		{"pi", "pi", "", true},
		{"pi:some-model", "pi", "some-model", true},
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

// TestProviderSpecParsing covers "<base>[:<model>][@<effort>]". Effort is
// stripped before the model split, so a spec carrying effort but no model still
// yields a usable base — the case that would otherwise produce a provider name
// of "claude@high" and fail the DefaultCommand switch with "unknown provider".
func TestProviderSpecParsing(t *testing.T) {
	for _, tc := range []struct{ spec, base, model, effort string }{
		{"claude", "claude", "", ""},
		{"claude:opus", "claude", "opus", ""},
		{"claude:opus@high", "claude", "opus", "high"},
		{"claude@max", "claude", "", "max"},
		{"codex:gpt-5.6-luna", "codex", "gpt-5.6-luna", ""},
		{"codex:gpt-5.6-sol@high", "codex", "gpt-5.6-sol", "high"},
		{"cursor", "cursor", "", ""},
		{"", "", "", ""},
	} {
		if got := Base(tc.spec); got != tc.base {
			t.Errorf("Base(%q) = %q, want %q", tc.spec, got, tc.base)
		}
		if got := Model(tc.spec); got != tc.model {
			t.Errorf("Model(%q) = %q, want %q", tc.spec, got, tc.model)
		}
		if got := Effort(tc.spec); got != tc.effort {
			t.Errorf("Effort(%q) = %q, want %q", tc.spec, got, tc.effort)
		}
	}
}
