package llmagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectTransient(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"at capacity", "Selected model is at capacity. Please try a different model.", true},
		{"overloaded", "Error: the model is overloaded", true},
		{"503", "upstream returned 503", true},
		{"529", "anthropic overloaded_error (529)", true},
		{"service unavailable", "503 Service Unavailable", true},
		// quota takes precedence: a real budget/rate-limit is not transient.
		{"quota wins over capacity", "you have exhausted your capacity for the month", false},
		{"rate limit 429", "429 too many requests", false},
		{"plain failure", "exec: \"codex\": executable file not found", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectTransient(c.in); got != c.want {
				t.Errorf("DetectTransient(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestRunnerRetriesTransient verifies that a transient capacity error triggers
// in-place retries of the SAME provider before failover, and that the exhausted
// provider gets the short capacity cooldown rather than the full blacklist.
func TestRunnerRetriesTransient(t *testing.T) {
	dir := t.TempDir()
	// flaky records each invocation and always fails with a transient error.
	flaky := preProbed("flaky", `printf x >> calls; echo "Selected model is at capacity. Please try a different model."; exit 1`)
	good := preProbed("good", `cat >/dev/null; echo ok`)
	r := &Runner{
		Agents:           []*Agent{flaky, good},
		Cooldowns:        NewCooldownTracker(),
		TransientBackoff: -1, // no wait in tests
	}

	res, err := r.Run(context.Background(), "p", dir, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Provider != "good" {
		t.Errorf("Provider = %q, want good", res.Provider)
	}

	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if want := 1 + defaultTransientRetries; len(calls) != want {
		t.Errorf("flaky invoked %d times, want %d (1 + %d retries)", len(calls), want, defaultTransientRetries)
	}

	rem := r.Cooldowns.Remaining("flaky")
	if rem <= 0 || rem > CapacityCooldown {
		t.Errorf("flaky cooldown = %v, want in (0, %v] (capacity, not blacklist)", rem, CapacityCooldown)
	}
	if rem > BlacklistCooldown {
		t.Errorf("flaky got blacklist-length cooldown %v; transient should be shorter", rem)
	}
}

// TestRunnerTransientThenSuccess verifies a provider that recovers on a retry
// wins without failing over.
func TestRunnerTransientThenSuccess(t *testing.T) {
	dir := t.TempDir()
	// Fails transiently on the first call, succeeds on the second.
	recovering := preProbed("recovering", `
n=$(cat n 2>/dev/null || echo 0); n=$((n+1)); echo $n > n
if [ "$n" -ge 2 ]; then cat >/dev/null; echo ok; else echo "model is overloaded"; exit 1; fi`)
	r := &Runner{
		Agents:           []*Agent{recovering},
		Cooldowns:        NewCooldownTracker(),
		TransientBackoff: -1,
	}
	res, err := r.Run(context.Background(), "p", dir, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Provider != "recovering" {
		t.Errorf("Provider = %q, want recovering", res.Provider)
	}
	if r.Cooldowns.IsCoolingDown("recovering") {
		t.Error("provider that recovered on retry should not be in cooldown")
	}
}
