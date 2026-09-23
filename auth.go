package llmagent

import "strings"

// authPatterns match a provider that failed because the credential it is
// holding is no longer usable: an expired login, a revoked or already-spent
// refresh token, a rejected key.
//
// This class is deliberately distinct from both quota and transient. A quota
// error clears on a schedule, a capacity error clears in seconds, and both are
// worth waiting out. An expired login clears when a human runs the provider's
// login command and not one moment sooner, so the useful thing the Runner can
// do is say which credential went stale rather than retry into it.
//
// Every entry is a substring of a message a provider actually emitted:
//
//	claude  "API Error: 401 Invalid API key · Please run /login"
//	        "Your session has expired. Please run /login to sign in again."
//	codex   "ERROR codex_login::auth::manager: Failed to refresh token: 401
//	        Unauthorized: {"error":{"message":"Your refresh token has already
//	        been used to generate a new access token. Please try signing in
//	        again.","type":"invalid_request_error","code":"refresh_token_reused"}}"
//
// A bare "401" or "unauthorized" is deliberately absent: both turn up inside
// unrelated payloads and inside agent transcript text, and the phrases here are
// specific enough without them.
var authPatterns = []string{
	"not logged in",
	"please run /login",
	"please log in",
	"please login",
	"login required",
	"login expired",
	"authentication failed",
	"authentication_error",
	"invalid api key",
	"invalid_api_key",
	"invalid_grant",
	"bad credentials",
	"credentials expired",
	"session expired",
	"session has expired",
	"token expired",
	"token has expired",
	"token revoked",
	"failed to refresh token",
	"refresh_token_reused",
	"sign in again",
	"signing in again",
	"re-authenticate",
	"reauthenticate",
	"401 unauthorized",
	"403 forbidden",
}

// DetectAuth reports whether output signals a credential the operator has to
// renew by hand, and if so returns the provider's own sentence describing it.
//
// Quota takes precedence, for the same reason it does in DetectTransient: a
// provider that says "usage limit reached" or "credit balance too low" recovers
// on a reset or a top-up, and both wordings overlap this vocabulary closely
// enough to misfile a wait as a page.
func DetectAuth(output string) (detail string, ok bool) {
	if _, isQuota := DetectQuota(output); isQuota {
		return "", false
	}
	lower := asciiLower(output)
	start, end := -1, 0
	for _, pat := range authPatterns {
		i := strings.Index(lower, pat)
		if i >= 0 && (start < 0 || i < start) {
			start, end = i, i+len(pat)
		}
	}
	if start < 0 {
		return "", false
	}
	return signalExcerpt(output, start, end), true
}

// maxSignalBefore and maxSignalAfter bound the excerpt DetectAuth reports,
// measured outward from the matched phrase rather than from the front of the
// output. Which end you truncate from is the whole point: claude runs under
// --output-format stream-json, so its stdout opens with an init event carrying
// the cwd, the session id and the full tool list — over 512 bytes of banner
// before the first byte of anything diagnostic — and codex opens with a
// timestamped log header. Truncating from the front reliably logs the preamble
// and drops the sentence naming the credential.
const (
	maxSignalBefore = 80
	maxSignalAfter  = 140
)

// signalExcerpt returns the text surrounding the match at [start,end), clipped
// at the enclosing JSON string quote or line break when one is within budget.
// When the budget runs out first the cut is moved to a space, so the excerpt
// never begins or ends mid-word.
func signalExcerpt(output string, start, end int) string {
	lo := start
	for lo > 0 && start-lo < maxSignalBefore && !isSignalBoundary(output[lo-1]) {
		lo--
	}
	if lo > 0 && !isSignalBoundary(output[lo-1]) {
		if i := strings.IndexByte(output[lo:start], ' '); i >= 0 {
			lo += i + 1
		}
	}
	hi := end
	for hi < len(output) && hi-end < maxSignalAfter && !isSignalBoundary(output[hi]) {
		hi++
	}
	if hi < len(output) && !isSignalBoundary(output[hi]) {
		if i := strings.LastIndexByte(output[end:hi], ' '); i >= 0 {
			hi = end + i
		}
	}
	excerpt := strings.Trim(output[lo:hi], " \t\r\n\\\",;:")
	// The window is cut on byte offsets and the budget can land inside a
	// multi-byte rune; drop any partial one rather than emit invalid UTF-8.
	return strings.ToValidUTF8(excerpt, "")
}

func isSignalBoundary(c byte) bool {
	return c == '\n' || c == '\r' || c == '"'
}

// asciiLower lowercases ASCII bytes only, so offsets into the result map one
// to one onto output. strings.ToLower cannot promise that — a handful of code
// points lowercase to a different byte length — and a shifted offset would
// slide the excerpt window off the phrase it is supposed to be centred on.
// Every entry in authPatterns is ASCII, so nothing else needs folding.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
