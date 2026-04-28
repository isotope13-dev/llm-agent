package llmagent

import (
	"testing"
	"time"
)

func TestDetectQuota(t *testing.T) {
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
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"resets in 8h24m6s", 8*time.Hour + 24*time.Minute + 6*time.Second},
		{"resets in 7h59m", 7*time.Hour + 59*time.Minute},
		{"resets in 8h", 8 * time.Hour},
		{"resets in 45m", 45 * time.Minute},
		{"resets in 30s", 30 * time.Second},
		{"resets in 5m30s", 5*time.Minute + 30*time.Second},
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
