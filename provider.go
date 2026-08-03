package llmagent

import "strings"

// A provider spec is "<base>[:<model>][@<effort>]" -- e.g. "claude:opus@high",
// "codex:gpt-5.6-luna", "cursor". Effort is stripped before the model split so
// "claude@high" (effort, no model) parses correctly rather than yielding a base
// of "claude@high".

// Base returns the provider name without any model or effort suffix.
// "gemini:gemini-2.5-pro@high" → "gemini"; "claude@max" → "claude".
func Base(provider string) string {
	spec, _, _ := strings.Cut(provider, "@")
	if base, _, ok := strings.Cut(spec, ":"); ok {
		return base
	}
	return spec
}

// Model returns the model suffix, or "" if none.
// "gemini:gemini-2.5-pro@high" → "gemini-2.5-pro"; "claude@max" → "".
func Model(provider string) string {
	spec, _, _ := strings.Cut(provider, "@")
	if _, model, ok := strings.Cut(spec, ":"); ok {
		return model
	}
	return ""
}

// Effort returns the effort suffix, or "" if none.
// "claude:opus@high" → "high"; "codex:gpt-5.6-sol" → "".
//
// Not validated here: the accepted set differs per provider (claude takes
// low/medium/high/xhigh/max; codex carries it as the model_reasoning_effort
// config key) and an unknown value is the provider CLI's error to report, not
// something to silently drop.
func Effort(provider string) string {
	if _, effort, ok := strings.Cut(provider, "@"); ok {
		return effort
	}
	return ""
}

// IsLocal reports whether the provider runs a local LLM and therefore
// needs a longer probe budget and shorter blacklist than cloud agents.
func IsLocal(provider string) bool {
	switch Base(provider) {
	case "opencode", "pi":
		return true
	}
	return false
}
