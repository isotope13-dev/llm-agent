package llmagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeInitEvent is the banner claude emits on stdout under
// --output-format stream-json before anything diagnostic. Reproduced at length
// on purpose: it is what a front-truncated error message ends up containing.
const claudeInitEvent = `{"type":"system","subtype":"init","cwd":"/data/rectifier/traits-dev",` +
	`"session_id":"7980ae4e-a224-4a7b-a857-f6dd5be1fe3f","tools":["Task","Bash","CronCreate",` +
	`"CronDelete","CronList","DesignSync","Edit","EnterWorktree","ExitWorktree","ListAgents",` +
	`"Monitor","NotebookEdit","PushNotification","Read","RemoteTrigger","ReportFindings",` +
	`"ScheduleWakeup","SendMessage","Skill","TaskOutput","TaskStop","ToolSearch","WebFetch",` +
	`"WebSearch","Workflow","Write"],"model":"opus","permissionMode":"bypassPermissions"}`

// claudeExpiredLogin is a full stream-json failure: banner, then the result
// event carrying the reason. Both lines are what claude 2.1 actually prints.
const claudeExpiredLogin = claudeInitEvent + "\n" +
	`{"type":"result","subtype":"error_during_execution","is_error":true,` +
	`"result":"API Error: 401 Invalid API key · Please run /login","session_id":"7980ae4e"}`

// codexSpentRefreshToken is copied from a rectifier journal entry: codex logs
// the refresh failure to stderr as a timestamped line wrapping the API's JSON.
const codexSpentRefreshToken = `2026-08-27T16:01:33.931196Z ERROR codex_login::auth::manager: ` +
	`Failed to refresh token: 401 Unauthorized: {
  "error": {
    "message": "Your refresh token has already been used to generate a new access token. Please try signing in again.",
    "type": "invalid_request_error",
    "param": null,
    "code": "refresh_token_reused"
  }
}`

func TestDetectAuth(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		want       bool
		wantDetail string
	}{
		{
			name:       "claude expired login",
			in:         claudeExpiredLogin,
			want:       true,
			wantDetail: "API Error: 401 Invalid API key · Please run /login",
		},
		{
			name:       "claude expired session",
			in:         `{"type":"result","is_error":true,"result":"Your session has expired. Please run /login to sign in again."}`,
			want:       true,
			wantDetail: "Your session has expired. Please run /login to sign in again.",
		},
		{
			name:       "codex spent refresh token",
			in:         codexSpentRefreshToken,
			want:       true,
			wantDetail: "2026-08-27T16:01:33.931196Z ERROR codex_login::auth::manager: Failed to refresh token: 401 Unauthorized: {",
		},
		{
			name:       "bare not-logged-in on stderr",
			in:         "cursor:cursor-grok-4.6-medium failed: exit status 1:\nNot logged in. Run cursor-agent login.",
			want:       true,
			wantDetail: "Not logged in. Run cursor-agent login.",
		},
		// Quota takes precedence: these recover without anyone logging in.
		{"usage limit is quota not auth", "Claude AI usage limit reached, resets at 9:05 AM", false, ""},
		{"429 is quota not auth", `{"error":{"message":"429 rate limit exceeded"}}`, false, ""},
		{"capacity is not auth", "Selected model is at capacity. Please try a different model.", false, ""},
		{"missing binary is not auth", `exec: "codex": executable file not found in $PATH`, false, ""},
		{"idle timeout is not auth", "pi idle timeout after 20m0s", false, ""},
		{"empty", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			detail, ok := DetectAuth(c.in)
			if ok != c.want {
				t.Fatalf("DetectAuth ok = %v, want %v (detail %q)", ok, c.want, detail)
			}
			if detail != c.wantDetail {
				t.Errorf("detail = %q, want %q", detail, c.wantDetail)
			}
		})
	}
}

// TestDetectAuthExcerptSkipsBanner is the point of the excerpt: the reason a
// stale claude login was undiagnosable for a day is that every caller-side
// truncation kept the head of the output, and the head is 500-odd bytes of
// init event. The detail has to carry the sentence, not the banner.
func TestDetectAuthExcerptSkipsBanner(t *testing.T) {
	detail, ok := DetectAuth(claudeExpiredLogin)
	if !ok {
		t.Fatal("DetectAuth did not fire on an expired claude login")
	}
	if strings.Contains(detail, `"type":"system"`) || strings.Contains(detail, "ToolSearch") {
		t.Errorf("detail carries the init banner: %q", detail)
	}
	if !strings.Contains(detail, "Please run /login") {
		t.Errorf("detail does not name the remedy: %q", detail)
	}
	if limit := maxSignalBefore + maxSignalAfter; len(detail) > limit {
		t.Errorf("detail is %d bytes, want <= %d", len(detail), limit)
	}
}

// TestDetectAuthExcerptStaysValidUTF8 covers a match landing far enough from
// the multi-byte separator that the window is cut on the byte budget instead
// of a quote.
func TestDetectAuthExcerptStaysValidUTF8(t *testing.T) {
	in := "not logged in " + strings.Repeat("ok ", 60) + "· tail"
	detail, ok := DetectAuth(in)
	if !ok {
		t.Fatal("DetectAuth did not fire")
	}
	if !utf8ValidString(detail) {
		t.Errorf("detail is not valid UTF-8: %q", detail)
	}
}

func utf8ValidString(s string) bool { return strings.ToValidUTF8(s, "") == s }

// TestDetectTransientSkipsAuth pins the precedence that stopped codex from
// spending its in-invoke retries re-presenting a token the server had already
// retired.
func TestDetectTransientSkipsAuth(t *testing.T) {
	if DetectTransient(codexSpentRefreshToken) {
		t.Error("a spent refresh token was classified transient")
	}
}

// TestRunnerAuthCooldownNamesCredential checks the end-to-end shape an operator
// sees: one invocation (no transient retries), the blacklist duration rather
// than a quota-length wait, and a reason that says which credential to renew.
func TestRunnerAuthCooldownNamesCredential(t *testing.T) {
	dir := t.TempDir()
	stale := preProbed("claude:opus@medium",
		`printf x >> calls; cat >/dev/null; printf '%s\n' `+shellQuote(claudeExpiredLogin)+`; exit 1`)
	good := preProbed("good", `cat >/dev/null; echo ok`)
	tracker := NewCooldownTracker()
	r := &Runner{Agents: []*Agent{stale, good}, Cooldowns: tracker, TransientBackoff: -1}

	res, err := r.Run(context.Background(), "p", dir, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Provider != "good" {
		t.Errorf("Provider = %q, want good", res.Provider)
	}

	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if len(calls) != 1 {
		t.Errorf("stale provider invoked %d times, want 1 (an expired login is not retryable)", len(calls))
	}

	statuses := tracker.Statuses([]string{"claude:opus@medium"})
	if len(statuses) != 1 {
		t.Fatalf("statuses = %v, want one entry", statuses)
	}
	reason := statuses[0].Reason
	if !strings.HasPrefix(reason, "auth: ") {
		t.Errorf("reason = %q, want an auth: prefix", reason)
	}
	if !strings.Contains(reason, "Please run /login") {
		t.Errorf("reason = %q, want it to name the remedy", reason)
	}
	if rem := tracker.Remaining("claude:opus@medium"); rem > BlacklistCooldown || rem < BlacklistCooldown-time.Minute {
		t.Errorf("remaining = %v, want ~%v", rem, BlacklistCooldown)
	}
}

// shellQuote wraps s for /bin/sh. The fixtures contain double quotes and a
// multi-byte separator, neither of which survives naive interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
