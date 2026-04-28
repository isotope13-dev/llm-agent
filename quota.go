package llmagent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DefaultCooldown is the cooldown applied when a quota error has no parseable
// reset time.
const DefaultCooldown = 45 * time.Minute

// QuotaError indicates a provider failed because of quota or rate-limit
// exhaustion. It is kept distinct from generic provider failures so callers
// can decide whether to retry, fail over, or wait for the cooldown to expire.
type QuotaError struct {
	Provider string
	Detail   string // e.g. "resets in 8h24m"
}

func (e *QuotaError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("out of quota (%s)", e.Detail)
	}
	return "out of quota"
}

var quotaPatterns = []string{
	"QuotaError",
	"quota",
	"rate limit",
	"rate_limit",
	"exhausted your capacity",
	"too many requests",
	"usage limit",
	"429",
}

// DetectQuota reports whether output contains a quota or rate-limit signal,
// and if so returns a human-readable detail string (e.g. "resets in 8h24m6s").
func DetectQuota(output string) (detail string, ok bool) {
	lower := strings.ToLower(output)
	for _, pat := range quotaPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return extractQuotaDetail(output), true
		}
	}
	return "", false
}

func extractQuotaDetail(output string) string {
	lower := strings.ToLower(output)
	for _, marker := range []string{"reset after ", "reset in ", "resets in "} {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			continue
		}
		start := idx + len(marker)
		end := start
		for end < len(output) && output[end] != '\n' && output[end] != '.' && output[end] != ')' {
			end++
		}
		if s := strings.TrimSpace(output[start:end]); s != "" {
			return "resets in " + s
		}
	}
	return ""
}

// durationRe matches Go-style durations like "8h24m6s", "24m6s", "6s", "8h".
var durationRe = regexp.MustCompile(`\d+[hms](?:\d+[hms])*`)

// ParseResetDuration parses a Go duration embedded in detail (e.g. "resets in
// 8h24m6s"). Returns DefaultCooldown when nothing parseable is found.
func ParseResetDuration(detail string) time.Duration {
	if detail == "" {
		return DefaultCooldown
	}
	m := durationRe.FindString(detail)
	if m == "" {
		return DefaultCooldown
	}
	d, err := time.ParseDuration(m)
	if err != nil || d <= 0 {
		return DefaultCooldown
	}
	return d
}
