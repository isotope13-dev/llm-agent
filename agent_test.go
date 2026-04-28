package llmagent

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// shellCmd returns a NewCmd hook that runs script via /bin/sh.
func shellCmd(script string) func(ctx context.Context, a *Agent) (*exec.Cmd, error) {
	return func(ctx context.Context, _ *Agent) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", script), nil
	}
}

// preProbed returns an agent that has already passed its probe.
func preProbed(provider, script string) *Agent {
	a := &Agent{Provider: provider, NewCmd: shellCmd(script)}
	a.probeOnce.Do(func() {}) // mark as done with nil error
	return a
}

func TestRunSuccess(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; echo '{"type":"result","result":"ok"}'`)
	res, err := a.Run(context.Background(), "prompt", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "result") {
		t.Errorf("Output = %q", res.Output)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v", res.Duration)
	}
}

func TestRunCapturesSessionID(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; echo '{"type":"start","sessionID":"abc-123"}'; echo '{"type":"end","session_id":"def-456"}'`)
	res, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != "def-456" {
		t.Errorf("SessionID = %q, want def-456", res.SessionID)
	}
}

func TestRunOnEvent(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; echo a; echo b; echo c`)
	var got []string
	_, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{
		OnEvent: func(line string) { got = append(got, line) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("OnEvent got %d lines, want 3 (%v)", len(got), got)
	}
}

func TestRunOnStderrFiltersNoise(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; echo 'YOLO mode enabled' >&2; echo 'real error' >&2; echo done`)
	var got []string
	_, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{
		OnStderr: func(line string) { got = append(got, line) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "real error") {
		t.Errorf("OnStderr = %v, want only 'real error'", got)
	}
}

func TestRunProcessFailureCapturesStderr(t *testing.T) {
	a := preProbed("mock", `echo bad >&2; exit 1`)
	_, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error %q should contain stderr tail", err)
	}
}

func TestRunIdleTimeout(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; sleep 5`)
	a.IdleTimeout = 100 * time.Millisecond
	a.Timeout = 30 * time.Second
	_, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
}

func TestRunCancellation(t *testing.T) {
	a := preProbed("mock", `cat >/dev/null; sleep 5`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := a.Run(ctx, "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error after cancel")
	}
}

func TestRunCursorWritesPromptFile(t *testing.T) {
	a := preProbed("cursor", `[ -f PROMPT.md ] && cat PROMPT.md && echo done`)
	dir := t.TempDir()
	res, err := a.Run(context.Background(), "hello cursor", dir, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "hello cursor") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestProbeSuccess(t *testing.T) {
	a := &Agent{Provider: "mock", NewCmd: shellCmd(`echo ok`)}
	if err := a.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// Subsequent probe is a no-op.
	a.NewCmd = shellCmd(`exit 1`)
	if err := a.Probe(context.Background()); err != nil {
		t.Fatalf("second Probe: %v", err)
	}
}

func TestProbeStartFails(t *testing.T) {
	a := &Agent{
		Provider: "mock",
		NewCmd: func(ctx context.Context, _ *Agent) (*exec.Cmd, error) {
			return exec.CommandContext(ctx, "/nonexistent/binary/xyz"), nil
		},
	}
	if err := a.Probe(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestProbeNoOutput(t *testing.T) {
	a := &Agent{Provider: "mock", NewCmd: shellCmd(`exit 0`)}
	if err := a.Probe(context.Background()); err == nil {
		t.Fatal("expected error for silent provider")
	}
}

func TestProbeTimeout(t *testing.T) {
	a := &Agent{
		Provider:     "mock",
		ProbeTimeout: 100 * time.Millisecond,
		NewCmd:       shellCmd(`cat >/dev/null; sleep 5`),
	}
	if err := a.Probe(context.Background()); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestProbeReset(t *testing.T) {
	a := &Agent{Provider: "mock", NewCmd: shellCmd(`exit 0`)}
	if err := a.Probe(context.Background()); err == nil {
		t.Fatal("expected first probe to fail")
	}
	a.NewCmd = shellCmd(`echo ok`)
	a.Reset()
	if err := a.Probe(context.Background()); err != nil {
		t.Fatalf("after Reset: %v", err)
	}
}

func TestRunFailsOnEmptyProvider(t *testing.T) {
	a := &Agent{}
	_, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected error for empty provider")
	}
}

// piMockScript emits a pi-shaped subprocess that:
//  1. Reads the prompt command line from stdin and echoes it to stdout.
//  2. Emits an agent_end event so the feeder advances.
//  3. Reads the get_state command line.
//  4. Emits a response with the given sessionFile, then exits.
const piMockScript = `
read prompt_line
printf '%s\n' "$prompt_line"
printf '{"type":"agent_end","messages":[]}\n'
read state_line
printf '{"type":"response","command":"get_state","success":true,"data":{"sessionFile":"%s"}}\n' "$1"
`

// TestRunPiProtocolFreshSession stubs pi for a fresh-session run and
// asserts the prompt is wrapped as a JSON command, agent_end signals
// completion, and the captured sessionFile rides back on Result.SessionID.
func TestRunPiProtocolFreshSession(t *testing.T) {
	piResponseGrace = 200 * time.Millisecond
	t.Cleanup(func() { piResponseGrace = 3 * time.Second })

	const sessionPath = "/tmp/pi-session-fresh.jsonl"
	a := preProbed("pi", "")
	a.NewCmd = func(ctx context.Context, _ *Agent) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", piMockScript, "sh", sessionPath), nil
	}
	var got []string
	res, err := a.Run(context.Background(), "do the thing", t.TempDir(), RunOptions{
		OnEvent: func(line string) { got = append(got, line) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("OnEvent got %d lines, want >=2: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"type":"prompt"`) || !strings.Contains(got[0], `"message":"do the thing"`) {
		t.Errorf("first echoed line doesn't look like an encoded pi prompt: %q", got[0])
	}
	if res.SessionID != sessionPath {
		t.Errorf("SessionID = %q, want %q", res.SessionID, sessionPath)
	}
}

// TestRunPiProtocolResume asserts the feeder issues a switch_session
// command before the prompt when opts.SessionID is non-empty, and that
// the captured session file is preserved on Result.
func TestRunPiProtocolResume(t *testing.T) {
	piResponseGrace = 200 * time.Millisecond
	t.Cleanup(func() { piResponseGrace = 3 * time.Second })

	const existingSession = "/tmp/pi-session-resume.jsonl"
	// This mock reads two control commands (switch_session + prompt),
	// echoes the first two lines, then emits agent_end + get_state response.
	script := `
read switch_line
printf '%s\n' "$switch_line"
read prompt_line
printf '%s\n' "$prompt_line"
printf '{"type":"agent_end"}\n'
read state_line
printf '{"type":"response","command":"get_state","success":true,"data":{"sessionFile":"%s"}}\n' "$1"
`
	a := preProbed("pi", "")
	a.NewCmd = func(ctx context.Context, _ *Agent) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", script, "sh", existingSession), nil
	}
	var got []string
	res, err := a.Run(context.Background(), "continue", t.TempDir(), RunOptions{
		SessionID: existingSession,
		OnEvent:   func(line string) { got = append(got, line) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("OnEvent got %d lines, want >=2: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"type":"switch_session"`) || !strings.Contains(got[0], existingSession) {
		t.Errorf("first echoed line should be switch_session for %s: %q", existingSession, got[0])
	}
	if !strings.Contains(got[1], `"type":"prompt"`) {
		t.Errorf("second echoed line should be prompt: %q", got[1])
	}
	if res.SessionID != existingSession {
		t.Errorf("SessionID = %q, want %q", res.SessionID, existingSession)
	}
}

// TestRunPiSessionFallsBackToRequested ensures that when pi never returns a
// get_state response (e.g. it died early), Run still echoes back the
// requested session ID so the caller's bookkeeping stays consistent.
func TestRunPiSessionFallsBackToRequested(t *testing.T) {
	piResponseGrace = 50 * time.Millisecond
	t.Cleanup(func() { piResponseGrace = 3 * time.Second })

	const sid = "/tmp/pi-session-fallback.jsonl"
	a := preProbed("pi", `
read switch_line
read prompt_line
printf '{"type":"agent_end"}\n'
`)
	res, err := a.Run(context.Background(), "x", t.TempDir(), RunOptions{SessionID: sid})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID != sid {
		t.Errorf("SessionID = %q, want fallback %q", res.SessionID, sid)
	}
}

func TestRunPropagatesProbeFailure(t *testing.T) {
	a := &Agent{Provider: "mock", NewCmd: shellCmd(`exit 0`)} // probe will fail (no output)
	_, err := a.Run(context.Background(), "p", t.TempDir(), RunOptions{})
	if err == nil {
		t.Fatal("expected probe failure to propagate")
	}
}
