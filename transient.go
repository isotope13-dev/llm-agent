package llmagent

import (
	"strings"
	"time"
)

// CapacityCooldown is applied to a provider that still returns a transient
// capacity/overload error after its in-invoke retries are exhausted. It is much
// shorter than a quota reset (DefaultCooldown) or the generic blacklist
// (BlacklistCooldown) because the condition — the model/server is momentarily
// "at capacity" or overloaded — typically clears within seconds to a couple of
// minutes, and the provider is otherwise healthy and authenticated.
const CapacityCooldown = 2 * time.Minute

// defaultTransientRetries is how many extra times Run re-invokes the SAME
// provider after a transient error before failing over. Total attempts on that
// provider = 1 + defaultTransientRetries.
const defaultTransientRetries = 2

// defaultTransientBackoff is the base delay before the first same-provider
// retry; it doubles each subsequent attempt (2s, then 4s). Kept short on
// purpose: a capacity blip that recovers in about a second should not cost a
// failover to a slow or unavailable sibling provider.
const defaultTransientBackoff = 2 * time.Second

// transientPatterns match provider errors that are short-lived and usually
// clear on a quick retry — the model/server is momentarily overloaded or "at
// capacity" — as opposed to quota exhaustion (a long, budgeted reset) or a hard
// failure (auth, missing binary, connection refused). A provider hitting one of
// these is still healthy, so the Runner retries it in place before moving on.
//
// Kept deliberately distinct from quotaPatterns: "exhausted your capacity" is a
// quota signal (long reset), whereas "at capacity" / "overloaded" are transient.
var transientPatterns = []string{
	"at capacity",
	"please try a different model",
	"overloaded",
	"service unavailable",
	"temporarily unavailable",
	"try again later",
	"503",
	"529",
}

// DetectTransient reports whether output signals a short-lived provider hiccup
// worth a brief same-provider retry rather than an immediate failover. Quota
// signals take precedence: a genuine quota/rate-limit error will not clear in a
// few seconds, so it is not treated as transient even if the wording overlaps.
func DetectTransient(output string) bool {
	if _, isQuota := DetectQuota(output); isQuota {
		return false
	}
	lower := strings.ToLower(output)
	for _, pat := range transientPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}
