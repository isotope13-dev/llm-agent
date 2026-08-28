package llmagent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultCooldown is the cooldown applied when a quota error has no parseable
// reset time. Two hours, not one: the providers that report a bare exhaustion
// with no reset -- cursor's "You're out of usage" is the standing example --
// are the ones whose limit is a plan or billing-period allowance rather than a
// rolling hourly window, so a shorter wait mostly buys another rejection. Each
// expiry costs one wasted invoke per provider variant, and nothing recovers
// sooner for having been asked more often. A provider that does publish a
// reset time never reaches this constant.
const DefaultCooldown = 2 * time.Hour

// minResetCooldown floors parsed reset times: a reset in the past (clock skew,
// or the window rolled over while the error was in flight) or a sub-minute
// server retry hint still costs one short cooldown rather than a hot retry loop.
const minResetCooldown = time.Minute

// maxResetCooldown caps parsed reset times at just over a weekly quota window;
// anything longer is almost certainly a misparse, so fall back to
// DefaultCooldown and let the retry rediscover the real reset.
const maxResetCooldown = 8 * 24 * time.Hour

// timeNow is stubbed in tests to make absolute reset times deterministic.
var timeNow = time.Now

// QuotaError indicates a provider failed because of quota or rate-limit
// exhaustion. It is kept distinct from generic provider failures so callers
// can decide whether to retry, fail over, or wait for the cooldown to expire.
type QuotaError struct {
	Provider string
	Detail   string // e.g. "resets in 8h24m" or "resets at 2026-07-15T15:45:00-04:00"
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
	"usage_limit",  // codex: usage_limit_reached error codes
	"usagelimit",   // codex: UsageLimitReachedError payloads
	"out of usage", // cursor: "You're out of usage" ActionRequiredError
	"429",
	// Prepaid-balance exhaustion. Not a rate limit, but the same shape from a
	// caller's view: the binary ran, the credential authenticated, the backend
	// answered, and the block clears when a human tops up rather than when
	// anything is retried. Classifying it here keeps it on the cooldown path
	// instead of the blacklist, and -- because probe verdicts latch for the
	// process lifetime -- stops a funded account from staying retired until
	// the next restart. OpenCode Zen: "APIError: Insufficient balance."
	"insufficient balance",
	"insufficient_balance",
	"insufficient funds",
	"payment required",
	"402",
}

// DetectQuota reports whether output contains a quota or rate-limit signal,
// and if so returns a human-readable detail string. When the output carries a
// reset time it is normalized to "resets in <duration>" or "resets at
// <RFC3339>", both of which ParseResetDuration understands.
func DetectQuota(output string) (detail string, ok bool) {
	lower := strings.ToLower(output)
	for _, pat := range quotaPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return extractQuotaDetail(output, lower), true
		}
	}
	return "", false
}

// extractQuotaDetail pulls the most precise reset signal available from a
// quota error, in order of preference:
//
//  1. an absolute time from the human message — codex renders its
//     UsageLimitReachedError as "Try again at Jul 15 3:45 PM." (with an
//     optional ", 2026" year for far-off resets);
//  2. a relative duration — gemini's "resets in 8h24m6s", OpenAI's
//     "try again in 20 seconds" / "in 1.234s", verbose "2 hours 30 minutes";
//  3. the structured "resets_at" field codex serializes into its JSON error
//     payloads (unix seconds/millis or RFC3339).
func extractQuotaDetail(output, lower string) string {
	now := timeNow()
	for _, marker := range []string{"try again at ", "resets at "} {
		if s := afterMarker(output, lower, marker); s != "" {
			if t, ok := parseClockTime(s, now); ok {
				return "resets at " + t.Format(time.RFC3339)
			}
		}
	}
	for _, marker := range []string{"reset after ", "reset in ", "resets in ", "try again in "} {
		s := afterMarker(output, lower, marker)
		if s == "" {
			continue
		}
		if d := parseHumanDuration(s); d > 0 {
			return "resets in " + d.String()
		}
		return "resets in " + s
	}
	if t, ok := parseResetsAtField(output); ok {
		return "resets at " + t.Format(time.RFC3339)
	}
	return ""
}

// afterMarker returns the text following marker up to the end of the phrase: a
// newline, quote (the message often sits inside a JSON string), closing paren,
// or a period that isn't a decimal point ("try again in 1.234s"). Capped at 80
// bytes so a marker in unrelated output can't drag in garbage.
func afterMarker(output, lower, marker string) string {
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := start
	for end < len(output) && end-start < 80 {
		c := output[end]
		if c == '\n' || c == '"' || c == ')' || c == '\\' {
			break
		}
		if c == '.' && (end+1 >= len(output) || output[end+1] < '0' || output[end+1] > '9') {
			break
		}
		end++
	}
	return strings.TrimSpace(output[start:end])
}

// clockLayouts match codex's reset-time rendering (strftime
// "%b %-d[, %Y] %-I:%M %p") plus a bare clock time for same-day resets.
var clockLayouts = []string{
	"Jan 2, 2006 3:04 PM",
	"Jan 2 3:04 PM",
	"3:04 PM",
	"15:04",
}

// ordinalDayRe matches an English ordinal suffix on a day number ("Sep 1st,
// 2026"). Codex renders far-off resets that way, and Go's reference layouts
// have no verb for it, so the suffix is stripped before parsing.
var ordinalDayRe = regexp.MustCompile(`(?i)\b(\d{1,2})(st|nd|rd|th)\b`)

// parseClockTime parses an absolute reset time in the host's local timezone
// (codex formats the timestamp on this same host). Layouts without a year or
// date resolve to the next future occurrence relative to now.
func parseClockTime(s string, now time.Time) (time.Time, bool) {
	s = ordinalDayRe.ReplaceAllString(s, "$1")
	for _, layout := range clockLayouts {
		t, err := time.ParseInLocation(layout, s, now.Location())
		if err != nil {
			continue
		}
		switch layout {
		case "3:04 PM", "15:04":
			t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			if !t.After(now) {
				t = t.Add(24 * time.Hour)
			}
		case "Jan 2 3:04 PM":
			t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			// A dateless render is always in the future; more than an hour in
			// the past means the message straddled a year boundary.
			if t.Before(now.Add(-time.Hour)) {
				t = t.AddDate(1, 0, 0)
			}
		}
		return t, true
	}
	return time.Time{}, false
}

// resetsAtRe matches the "resets_at" field of codex's UsageLimitReachedError /
// RateLimitWindow JSON payloads: unix seconds, unix millis, or an RFC3339
// string. The first match wins — the top-level (blocking) window serializes
// before the nested rate_limits windows.
var resetsAtRe = regexp.MustCompile(`"resets_at"\s*:\s*(?:"([^"]+)"|(\d{9,13}))`)

func parseResetsAtField(output string) (time.Time, bool) {
	m := resetsAtRe.FindStringSubmatch(output)
	if m == nil {
		return time.Time{}, false
	}
	if m[1] != "" {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return t.Local(), true
		}
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if n > 1e12 { // millisecond precision
		return time.UnixMilli(n), true
	}
	return time.Unix(n, 0), true
}

// durationTokenRe matches one component of a spelled-out or Go-style duration:
// "2 hours", "30 minutes", "20 seconds", "8h", "24m", "1.234s", "500ms".
var durationTokenRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(days?|hours?|hrs?|minutes?|mins?|milliseconds?|seconds?|secs?|ms|[dhms])`)

// parseHumanDuration sums every duration token in s, accepting both Go-style
// ("8h24m6s") and spelled-out ("2 hours 30 minutes") forms. Returns 0 when no
// token parses.
func parseHumanDuration(s string) time.Duration {
	var total time.Duration
	for _, m := range durationTokenRe.FindAllStringSubmatch(s, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		unit := strings.ToLower(m[2])
		var scale time.Duration
		switch {
		case strings.HasPrefix(unit, "ms") || strings.HasPrefix(unit, "milli"):
			scale = time.Millisecond
		case unit[0] == 'd':
			scale = 24 * time.Hour
		case unit[0] == 'h':
			scale = time.Hour
		case unit[0] == 'm':
			scale = time.Minute
		case unit[0] == 's':
			scale = time.Second
		}
		total += time.Duration(v * float64(scale))
	}
	return total
}

// ParseResetDuration converts a detail string from DetectQuota into a cooldown:
// "resets at <RFC3339>" yields the time remaining until that instant, and
// "resets in <duration>" (or any embedded duration) yields that duration.
// Results are floored at one minute; implausible or unparseable values fall
// back to DefaultCooldown.
func ParseResetDuration(detail string) time.Duration {
	if detail == "" {
		return DefaultCooldown
	}
	if s, ok := strings.CutPrefix(detail, "resets at "); ok {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return DefaultCooldown
		}
		return clampReset(t.Sub(timeNow()))
	}
	if d := parseHumanDuration(detail); d > 0 {
		return clampReset(d)
	}
	return DefaultCooldown
}

func clampReset(d time.Duration) time.Duration {
	if d > maxResetCooldown {
		return DefaultCooldown
	}
	if d < minResetCooldown {
		return minResetCooldown
	}
	return d
}
