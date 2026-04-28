package llmagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunnerSuccess(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; echo ok`)
	r := &Runner{Agents: []*Agent{a}}
	res, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Provider != "mock" {
		t.Errorf("Provider = %q", res.Provider)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestRunnerFailover(t *testing.T) {
	bad := preProbed("bad", `exit 1`)
	good := preProbed("good", `cat >/dev/null; echo ok`)
	r := &Runner{Agents: []*Agent{bad, good}, Cooldowns: NewCooldownTracker()}
	res, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Provider != "good" {
		t.Errorf("Provider = %q, want good", res.Provider)
	}
	if !r.Cooldowns.IsCoolingDown("bad") {
		t.Error("expected 'bad' to be in cooldown after failure")
	}
	if r.Cooldowns.IsCoolingDown("good") {
		t.Error("expected 'good' not in cooldown after success")
	}
}

func TestRunnerAllFail(t *testing.T) {
	a := preProbed("a", `exit 1`)
	b := preProbed("b", `exit 1`)
	r := &Runner{Agents: []*Agent{a, b}}
	_, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "all providers failed") {
		t.Errorf("error = %v", err)
	}
}

func TestRunnerNoAgents(t *testing.T) {
	r := &Runner{}
	_, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunnerSkipsCoolingDown(t *testing.T) {
	a := preProbed("a", `exit 1`) // would fail if tried
	b := preProbed("b", `cat >/dev/null; echo ok`)
	tracker := NewCooldownTracker()
	tracker.Set("a", time.Hour, "test")
	r := &Runner{Agents: []*Agent{a, b}, Cooldowns: tracker}

	res, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Provider != "b" {
		t.Errorf("Provider = %q, want b", res.Provider)
	}
}

func TestRunnerQuotaErrorParsesReset(t *testing.T) {
	a := preProbed("a", `echo "TerminalQuotaError: quota exhausted, resets in 2h30m" >&2; exit 2`)
	tracker := NewCooldownTracker()
	r := &Runner{Agents: []*Agent{a}, Cooldowns: tracker}
	_, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	rem := tracker.Remaining("a")
	if rem < 2*time.Hour+29*time.Minute || rem > 2*time.Hour+31*time.Minute {
		t.Errorf("remaining = %v, want ~2h30m", rem)
	}
}

func TestRunnerOnCooldown(t *testing.T) {
	a := preProbed("a", `exit 1`)
	tracker := NewCooldownTracker()
	var fired bool
	r := &Runner{
		Agents:     []*Agent{a},
		Cooldowns:  tracker,
		OnCooldown: func(string, time.Duration, string) { fired = true },
	}
	if _, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{}); err == nil {
		t.Fatal("expected runner error")
	}
	if !fired {
		t.Error("OnCooldown not fired")
	}
}

func TestRunnerReusesSession(t *testing.T) {
	// Provider that emits sessionID on every line.
	a := preProbed("claude", `cat >/dev/null; echo '{"sessionID":"sess-1"}'`)
	r := &Runner{Agents: []*Agent{a}}
	res, err := r.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q", res.SessionID)
	}
	if got := r.session("claude"); got != "sess-1" {
		t.Errorf("runner session = %q, want sess-1", got)
	}
}

func TestRunnerCancellationAborts(t *testing.T) {
	a := preProbed("a", `cat >/dev/null; sleep 5`)
	r := &Runner{Agents: []*Agent{a}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := r.Run(ctx, "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "cancel") {
		t.Errorf("err = %v, want cancellation-flavored", err)
	}
}
