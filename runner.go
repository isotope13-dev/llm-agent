package llmagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Runner invokes a list of agents in order, failing over from one to the next
// when an agent errors. Quota-style errors put the agent in cooldown so
// sibling Runners sharing the same CooldownTracker skip it.
//
// A Runner is safe for concurrent use and tracks its own per-provider
// session IDs across invocations.
type Runner struct {
	// Cooldowns is consulted before every Run; pass nil to disable.
	Cooldowns *CooldownTracker

	// Logger receives structured progress events. Defaults to slog.Default.
	Logger *slog.Logger

	// OnCooldown, if non-nil, is called whenever a provider is put into
	// cooldown — useful for surfacing the event in a UI.
	OnCooldown func(provider string, d time.Duration, reason string)

	sessions map[string]string // baseProvider → last session ID

	// Agents are the providers to try, in order.
	Agents []*Agent

	// LocalCooldown overrides the blacklist duration for local providers
	// (opencode, pi). Zero means 15 min.
	LocalCooldown time.Duration

	// TransientBackoff overrides the base delay before a same-provider retry
	// on a transient (capacity/overload) error. Zero means defaultTransientBackoff.
	// A negative value disables the wait entirely (used by tests).
	TransientBackoff time.Duration

	mu sync.Mutex
}

// RunResult is the outcome of a successful Runner.Run.
type RunResult struct {
	Provider  string
	Output    string
	SessionID string
	Duration  time.Duration
}

// Run tries each agent in turn. The first one to succeed wins. If every
// agent is currently cooling down, Run waits for the soonest expiry before
// retrying.
func (r *Runner) Run(ctx context.Context, prompt, workdir string, opts RunOptions) (*RunResult, error) {
	if len(r.Agents) == 0 {
		return nil, errors.New("llmagent: Runner has no agents")
	}
	start := time.Now()

	agents, err := r.waitForAvailable(ctx)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, ag := range agents {
		callOpts := opts
		if callOpts.SessionID == "" {
			callOpts.SessionID = r.session(ag.Provider)
		}
		res, err := r.runWithRetry(ctx, ag, prompt, workdir, callOpts)
		if err == nil {
			r.setSession(ag.Provider, res.SessionID)
			if r.Cooldowns != nil {
				r.Cooldowns.Clear(ag.Provider)
			}
			return &RunResult{
				Provider:  ag.Provider,
				Output:    res.Output,
				SessionID: res.SessionID,
				Duration:  time.Since(start),
			}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		r.maybeCooldown(ag.Provider, err)
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

// runWithRetry invokes one agent, retrying it in place on a transient
// capacity/overload error (which typically clears within seconds) before the
// caller fails over to the next provider. Quota and hard errors are returned
// immediately so the normal failover + cooldown path handles them — a genuine
// quota reset or a missing binary will not recover in a few seconds.
func (r *Runner) runWithRetry(ctx context.Context, ag *Agent, prompt, workdir string, opts RunOptions) (*Result, error) {
	for attempt := 0; ; attempt++ {
		if attempt == 0 {
			r.logger().Info("llmagent runner trying", slog.String("provider", ag.Provider))
		}
		res, err := ag.Run(ctx, prompt, workdir, opts)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		if attempt >= defaultTransientRetries || !DetectTransient(err.Error()) {
			return nil, err
		}
		backoff := (r.transientBackoff()) << attempt
		r.logger().Warn("llmagent transient provider error; retrying same provider",
			slog.String("provider", ag.Provider),
			slog.Int("attempt", attempt+1),
			slog.Duration("backoff", backoff),
			slog.String("error", truncate(err.Error(), 120)))
		if backoff > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
}

// transientBackoff returns the base same-provider retry delay: the configured
// override, or the default. A negative override means "no wait" (tests).
func (r *Runner) transientBackoff() time.Duration {
	if r.TransientBackoff < 0 {
		return 0
	}
	if r.TransientBackoff == 0 {
		return defaultTransientBackoff
	}
	return r.TransientBackoff
}

// waitForAvailable returns the slice of agents to try. If every agent is
// cooling down, it sleeps until the soonest one expires (or ctx fires) and
// then re-queries. The wait is bounded by ctx — callers wanting a deadline
// should pass one.
func (r *Runner) waitForAvailable(ctx context.Context) ([]*Agent, error) {
	if r.Cooldowns == nil {
		return r.Agents, nil
	}
	names := r.providerNames()
	selected := r.Cooldowns.Select(names)

	if rem := r.Cooldowns.Remaining(selected[0]); rem > 0 {
		r.logger().Info("llmagent runner all cooling, waiting",
			slog.String("provider", selected[0]),
			slog.Duration("wait", rem),
			slog.String("statuses", FormatStatuses(r.Cooldowns.Statuses(names))))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(rem):
		}
		// After waiting, drop any provider still inside its window — trying it
		// again would just reset the timer to the full default cooldown.
		selected = r.Cooldowns.Select(names)
	}
	return r.byName(selected), nil
}

func (r *Runner) providerNames() []string {
	names := make([]string, len(r.Agents))
	for i, a := range r.Agents {
		names[i] = a.Provider
	}
	return names
}

func (r *Runner) byName(names []string) []*Agent {
	index := make(map[string]*Agent, len(r.Agents))
	for _, a := range r.Agents {
		index[a.Provider] = a
	}
	out := make([]*Agent, 0, len(names))
	for _, n := range names {
		if a := index[n]; a != nil {
			out = append(out, a)
		}
	}
	return out
}

func (r *Runner) maybeCooldown(provider string, err error) {
	if r.Cooldowns == nil {
		return
	}
	if detail, ok := DetectQuota(err.Error()); ok {
		d := ParseResetDuration(detail)
		reason := "quota"
		if detail != "" {
			reason = "quota: " + detail
		}
		r.setCooldown(provider, d, reason)
		return
	}
	if DetectTransient(err.Error()) {
		// Survived its in-invoke retries but still overloaded: bench it briefly
		// (not the full blacklist) so the next batch can pick it right back up.
		r.setCooldown(provider, CapacityCooldown, "capacity: "+truncate(err.Error(), 80))
		return
	}
	d := BlacklistCooldown
	if IsLocal(provider) {
		if r.LocalCooldown > 0 {
			d = r.LocalCooldown
		} else {
			d = 15 * time.Minute
		}
	}
	r.setCooldown(provider, d, "blacklisted: "+truncate(err.Error(), 80))
}

func (r *Runner) setCooldown(provider string, d time.Duration, reason string) {
	r.Cooldowns.Set(provider, d, reason)
	r.logger().Info("llmagent cooldown",
		slog.String("provider", provider),
		slog.Duration("duration", d),
		slog.String("reason", reason))
	if r.OnCooldown != nil {
		r.OnCooldown(provider, d, reason)
	}
}

func (r *Runner) session(provider string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[Base(provider)]
}

func (r *Runner) setSession(provider, sid string) {
	if sid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = make(map[string]string)
	}
	r.sessions[Base(provider)] = sid
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
