package llmagent

import (
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

// BlacklistCooldown is applied to providers that fail with a non-quota error
// (connection refused, auth, timeout). It is shorter than the typical quota
// reset because the underlying problem is usually transient.
const BlacklistCooldown = 20 * time.Minute

// CooldownStatus is a snapshot of one provider's cooldown.
type CooldownStatus struct {
	Provider  string
	Reason    string
	Remaining time.Duration
}

func (s CooldownStatus) String() string {
	return fmt.Sprintf("%s=%s (%s)", s.Provider, s.Remaining.Truncate(time.Second), s.Reason)
}

// FormatStatuses joins a status slice into a comma-separated summary.
func FormatStatuses(statuses []CooldownStatus) string {
	parts := make([]string, len(statuses))
	for i, s := range statuses {
		parts[i] = s.String()
	}
	return strings.Join(parts, ", ")
}

type cooldownEntry struct {
	until  time.Time
	reason string
}

// CooldownTracker remembers when each provider's cooldown expires.
// It is safe for concurrent use.
type CooldownTracker struct {
	entries map[string]cooldownEntry
	now     func() time.Time // overridable for tests
	mu      sync.Mutex
}

// NewCooldownTracker returns an empty tracker.
func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{
		entries: make(map[string]cooldownEntry),
		now:     time.Now,
	}
}

// Set puts provider into cooldown for d, recording reason.
func (c *CooldownTracker) Set(provider string, d time.Duration, reason string) {
	c.mu.Lock()
	c.entries[provider] = cooldownEntry{until: c.now().Add(d), reason: reason}
	c.mu.Unlock()
}

// Clear removes any active cooldown for provider, including an account-scoped
// one on its base: a provider that just answered is proof its account works, so
// leaving the vendor-wide entry in place would bench its siblings on stale
// evidence.
func (c *CooldownTracker) Clear(provider string) {
	c.mu.Lock()
	delete(c.entries, provider)
	delete(c.entries, Base(provider))
	c.mu.Unlock()
}

// lookup resolves a provider's cooldown, preferring an entry for the exact spec
// and falling back to one keyed on its base.
//
// The fallback is what makes a cooldown account-scoped. A quota belongs to an
// ACCOUNT, not to a model spec: "claude:opus@low", "claude:sonnet@low" and
// "claude:sonnet@high" are one OAuth session, and the four "cursor:..." specs
// across the four tier chains are one subscription. Keyed per spec, a single
// exhausted account had to be rediscovered once per spec that addressed it —
// on 2026-09-01 one expired Claude credential cost 41 separate blacklistings
// and ~45 wasted invocations before every spelling of it had been benched.
// Callers record those failures under Base(provider); everything a chain asks
// about that vendor then reads the one entry.
func lookup(entries map[string]cooldownEntry, provider string) (cooldownEntry, bool) {
	if e, ok := entries[provider]; ok {
		return e, true
	}
	e, ok := entries[Base(provider)]
	return e, ok
}

// IsCoolingDown reports whether provider's cooldown has not yet expired.
func (c *CooldownTracker) IsCoolingDown(provider string) bool {
	return c.Remaining(provider) > 0
}

// Remaining returns time left on the cooldown, or 0 if none.
func (c *CooldownTracker) Remaining(provider string) time.Duration {
	c.mu.Lock()
	e, ok := lookup(c.entries, provider)
	c.mu.Unlock()
	if !ok {
		return 0
	}
	if rem := e.until.Sub(c.now()); rem > 0 {
		return rem
	}
	return 0
}

// Statuses returns a status for every provider in the input list that is
// currently cooling down, in input order.
func (c *CooldownTracker) Statuses(providers []string) []CooldownStatus {
	now := c.now()
	c.mu.Lock()
	snap := make(map[string]cooldownEntry, len(c.entries))
	maps.Copy(snap, c.entries)
	c.mu.Unlock()

	var out []CooldownStatus
	for _, p := range providers {
		if e, ok := lookup(snap, p); ok {
			if rem := e.until.Sub(now); rem > 0 {
				out = append(out, CooldownStatus{Provider: p, Remaining: rem, Reason: e.reason})
			}
		}
	}
	return out
}

// Select reorders providers so that those not in cooldown come first.
// If every provider is cooling down, the soonest-to-expire is returned first
// so callers can wait on the most-recoverable one.
func (c *CooldownTracker) Select(providers []string) []string {
	now := c.now()
	c.mu.Lock()
	snap := make(map[string]cooldownEntry, len(c.entries))
	maps.Copy(snap, c.entries)
	c.mu.Unlock()

	var available, cooling []string
	for _, p := range providers {
		if e, ok := lookup(snap, p); ok && now.Before(e.until) {
			cooling = append(cooling, p)
		} else {
			available = append(available, p)
		}
	}
	if len(available) > 0 {
		return available
	}
	sort.SliceStable(cooling, func(i, j int) bool {
		ei, _ := lookup(snap, cooling[i])
		ej, _ := lookup(snap, cooling[j])
		return ei.until.Before(ej.until)
	})
	return cooling
}
