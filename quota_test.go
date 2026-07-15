package llmagent

import (
	"fmt"
	"testing"
	"time"
)

// fixNow pins timeNow for the duration of a test so absolute reset times are
// deterministic. Returns the pinned instant.
func fixNow(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	now := time.Date(2026, time.July, 14, 15, 30, 0, 0, loc)
	orig := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = orig })
	return now
}

func TestDetectQuota(t *testing.T) {
	now := fixNow(t)
	tests := []struct {
		name       string
		input      string
		wantDetail string
		want       bool
	}{
		{
			name:       "gemini quota with reset",
			input:      "TerminalQuotaError: You have exhausted your capacity on this model. Your quota will reset after 8h24m6s.",
			want:       true,
			wantDetail: "resets in 8h24m6s",
		},
		{
			// Codex renders UsageLimitReachedError with strftime "%b %-d %-I:%M %p".
			name:       "codex usage limit with date",
			input:      `codex failed: exit status 1:` + "\n" + `{"type":"error","message":"You've hit your usage limit. Try again at Jul 15 3:45 PM."}`,
			want:       true,
			wantDetail: "resets at 2026-07-15T15:45:00-04:00",
		},
		{
			name:       "codex usage limit with year",
			input:      "You've hit your usage limit. Try again at Jul 15, 2027 3:45 PM.",
			want:       true,
			wantDetail: "resets at 2027-07-15T15:45:00-04:00",
		},
		{
			name:       "codex usage limit same-day clock time",
			input:      "You've hit your usage limit. Try again at 6:12 PM.",
			want:       true,
			wantDetail: "resets at 2026-07-14T18:12:00-04:00",
		},
		{
			name:       "clock time already past rolls to tomorrow",
			input:      "You've hit your usage limit. Try again at 9:05 AM.",
			want:       true,
			wantDetail: "resets at 2026-07-15T09:05:00-04:00",
		},
		{
			name:       "codex structured resets_at unix seconds",
			input:      `stream error: 429 {"plan_type":"plus","resets_at":` + fmt.Sprint(now.Add(97*time.Minute).Unix()) + `,"rate_limits":{"primary":{"used_percent":100.0,"window_minutes":300,"resets_at":1789999999}}}`,
			want:       true,
			wantDetail: "resets at " + time.Unix(now.Add(97*time.Minute).Unix(), 0).Format(time.RFC3339),
		},
		{
			name:       "codex structured resets_at rfc3339",
			input:      `usage_limit_reached: {"resets_at":"2026-07-15T02:00:00Z"}`,
			want:       true,
			wantDetail: "resets at " + time.Date(2026, time.July, 15, 2, 0, 0, 0, time.UTC).Local().Format(time.RFC3339),
		},
		{
			name:       "openai spelled-out retry hint",
			input:      "Rate limit reached for gpt-5 in organization org-x. Please try again in 20 seconds.",
			want:       true,
			wantDetail: "resets in 20s",
		},
		{
			name:       "openai decimal retry hint",
			input:      "429 Too Many Requests: try again in 1.234s",
			want:       true,
			wantDetail: "resets in 1.234s",
		},
		{
			name:       "verbose hours and minutes",
			input:      "usage limit exceeded, try again in 2 hours 30 minutes",
			want:       true,
			wantDetail: "resets in 2h30m0s",
		},
		{name: "rate limit", input: "Error: rate limit exceeded, too many requests", want: true},
		{name: "429", input: "HTTP 429: Too Many Requests", want: true},
		{name: "normal error", input: "connection refused", want: false},
		{name: "empty", input: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail, ok := DetectQuota(tt.input)
			if ok != tt.want {
				t.Fatalf("DetectQuota ok=%v, want %v", ok, tt.want)
			}
			if tt.wantDetail != "" && detail != tt.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tt.wantDetail)
			}
		})
	}
}

func TestQuotaErrorMessage(t *testing.T) {
	if got := (&QuotaError{Provider: "gemini", Detail: "resets in 8h"}).Error(); got != "out of quota (resets in 8h)" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&QuotaError{Provider: "gemini"}).Error(); got != "out of quota" {
		t.Errorf("Error() = %q", got)
	}
}

func TestParseResetDuration(t *testing.T) {
	now := fixNow(t)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"resets in 8h24m6s", 8*time.Hour + 24*time.Minute + 6*time.Second},
		{"resets in 7h59m", 7*time.Hour + 59*time.Minute},
		{"resets in 8h", 8 * time.Hour},
		{"resets in 45m", 45 * time.Minute},
		{"resets in 30s", minResetCooldown}, // floored: no hot retry loops
		{"resets in 5m30s", 5*time.Minute + 30*time.Second},
		{"resets in 2 hours 30 minutes", 2*time.Hour + 30*time.Minute},
		{"resets at " + now.Add(3*time.Hour).Format(time.RFC3339), 3 * time.Hour},
		{"resets at " + now.Add(-time.Hour).Format(time.RFC3339), minResetCooldown}, // already reset
		{"resets at " + now.AddDate(1, 0, 0).Format(time.RFC3339), DefaultCooldown}, // implausibly far: misparse guard
		{"resets at not-a-timestamp", DefaultCooldown},
		{"", DefaultCooldown},
		{"some random text", DefaultCooldown},
		{"quota will reset after 2h30m0s please wait", 2*time.Hour + 30*time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ParseResetDuration(tt.in); got != tt.want {
				t.Errorf("ParseResetDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
