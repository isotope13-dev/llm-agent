// Package llmagent invokes coding-agent CLIs (claude, agy, codex,
// opencode, pi, cursor, muse) as subprocesses, streams their stream-json
// output, and recovers from quota and rate-limit failures.
//
// The unit of work is an [Agent]: one named provider with everything
// needed to build its command line. Call [Agent.Run] with a prompt and
// a working directory to get the agent's stdout. A first call probes
// the binary; subsequent calls reuse the probe.
//
// To fail over across providers, wrap a slice of agents in a [Runner].
// The runner skips providers in [CooldownTracker] cooldown and retries
// the next one.
package llmagent
