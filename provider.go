package llmagent

import "strings"

// Base returns the provider name without any model suffix.
// "gemini:gemini-2.5-pro" → "gemini"; "claude" → "claude".
func Base(provider string) string {
	if base, _, ok := strings.Cut(provider, ":"); ok {
		return base
	}
	return provider
}

// Model returns the model suffix, or "" if none.
// "gemini:gemini-2.5-pro" → "gemini-2.5-pro"; "claude" → "".
func Model(provider string) string {
	if _, model, ok := strings.Cut(provider, ":"); ok {
		return model
	}
	return ""
}

// IsLocal reports whether the provider runs a local LLM and therefore
// needs a longer probe budget and shorter blacklist than cloud agents.
func IsLocal(provider string) bool {
	switch Base(provider) {
	case "opencode", "crush", "codex-oss":
		return true
	}
	return false
}
