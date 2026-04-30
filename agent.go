package llmagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrIdleTimeout is returned by Run when the subprocess produces no stdout
// for longer than Agent.IdleTimeout.
var ErrIdleTimeout = errors.New("idle timeout")

// Default time limits used when an Agent leaves a field zero.
const (
	defaultProbeTimeout      = 120 * time.Second
	defaultLocalProbeTimeout = 5 * time.Minute
)

// Agent is one named LLM CLI tool ready to be invoked. The zero value is not
// useful; set Provider at minimum.
//
// An Agent is safe for concurrent use: it probes once, lazily, and the probe
// outcome is shared by every caller. Sessions belong to the Run call, not the
// Agent — see RunOptions.SessionID.
//
// Field order minimises GC pointer-scan footprint; logical groupings are in
// the field comments.
type Agent struct {
	// NewCmd builds the command for one invocation. Defaults to DefaultCommand.
	// Override to inject mocks in tests or to support a new provider.
	NewCmd func(ctx context.Context, agent *Agent) (*exec.Cmd, error)

	// Logger receives structured progress events. Defaults to slog.Default.
	Logger *slog.Logger

	// probeErr stores the result of the lazy first-call probe.
	probeErr error

	// Provider is the agent name, optionally with a model suffix:
	// "claude", "gemini:gemini-2.5-pro", "opencode:kimi-k2".
	Provider string

	// TmpDir, when non-empty, is exported as TMPDIR/TMP/TEMP to the
	// subprocess so its scratch files land in a known location.
	TmpDir string

	// IncludeDirs are directories the agent is allowed to read or write.
	// Used by providers that take a directory allow-list (gemini, codex).
	IncludeDirs []string

	// Env is appended to the subprocess environment after TMPDIR overrides.
	// Each entry must be "KEY=VALUE".
	Env []string

	// Timeout is the wall-clock limit for one Run. Zero means no limit.
	Timeout time.Duration

	// IdleTimeout is the maximum gap between stdout lines. Zero means no
	// limit; the subprocess can stall indefinitely.
	IdleTimeout time.Duration

	// ProbeTimeout overrides the first-byte timeout used by Probe.
	// Zero picks 75s for cloud providers and 5 min for local ones.
	ProbeTimeout time.Duration

	probeOnce sync.Once
}

// RunOptions are per-call settings.
type RunOptions struct {
	// OnEvent is called once per non-empty stdout line. Lines are usually
	// stream-json; the caller is free to parse or ignore them.
	OnEvent func(line string)

	// OnStderr is called once per stderr line that is not provider-startup
	// noise (see IsStderrNoise).
	OnStderr func(line string)

	// SessionID, if non-empty, asks the provider to resume that session.
	// Providers that don't support resumption (codex, gemini) ignore
	// this field.
	SessionID string
}

// Result is the outcome of a successful Run.
type Result struct {
	Output    string        // captured stdout
	SessionID string        // last session ID parsed from the stream
	Duration  time.Duration // wall time of the invocation
}

const (
	maxStdoutBytes = 8 * 1024 * 1024 // captured Output cap
	maxStderrBytes = 256 * 1024      // tail of stderr included in error
	idleTickPeriod = 5 * time.Second
)

// Run invokes the agent with prompt in workdir. It probes the binary on
// first call.
func (a *Agent) Run(ctx context.Context, prompt, workdir string, opts RunOptions) (*Result, error) {
	if a.Provider == "" {
		return nil, errors.New("llmagent: Agent.Provider is empty")
	}
	if err := a.Probe(ctx); err != nil {
		return nil, err
	}

	start := time.Now()
	cmd, err := a.buildCmd(ctx, workdir, prompt, opts.SessionID)
	if err != nil {
		return nil, err
	}

	pipes, err := makePipes(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", a.Provider, err)
	}
	a.logger().Info("llmagent invoke",
		slog.String("provider", a.Provider),
		slog.Int("pid", cmd.Process.Pid),
		slog.Int("prompt_bytes", len(prompt)),
		slog.String("workdir", workdir))

	stderrTail := &strings.Builder{}
	stderrDone := streamStderr(stderrTail, pipes.stderr, opts.OnStderr)
	stream := &streamState{lastActive: time.Now()}

	var sigs *piSignals
	if Base(a.Provider) == "pi" {
		sigs = newPiSignals(pipes.stdin, a.logger())
		go a.feedPi(ctx, pipes.stdin, prompt, opts.SessionID, sigs)
	} else {
		go a.feedStdin(pipes.stdin, prompt)
	}
	stdoutDone := streamStdout(stream, pipes.stdout, opts.OnEvent, sigs)

	timeoutCtx, cancel := a.withTimeout(ctx)
	defer cancel()
	tick := time.NewTicker(idleTickPeriod)
	defer tick.Stop()

	for {
		select {
		case <-stdoutDone:
			return a.finish(cmd, start, stream, stderrTail, stderrDone, sigs, opts.SessionID)
		case <-tick.C:
			if a.idleExceeded(stream) {
				killAndReap(cmd)
				return nil, fmt.Errorf("%s %w after %v", a.Provider, ErrIdleTimeout, a.IdleTimeout)
			}
		case <-timeoutCtx.Done():
			killAndReap(cmd)
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s cancelled: %w", a.Provider, ctx.Err())
			}
			return nil, fmt.Errorf("%s timed out after %v", a.Provider, a.Timeout)
		}
	}
}

// buildCmd materialises the *exec.Cmd for this invocation: resolves resume
// args, sets the workdir, and writes Cursor's PROMPT.md side-channel.
func (a *Agent) buildCmd(ctx context.Context, workdir, prompt, sessionID string) (*exec.Cmd, error) {
	cmd, err := a.newCmd(ctx)
	if err != nil {
		return nil, err
	}
	if extra := resumeArgs(a.Provider, sessionID); extra != nil {
		cmd.Args = append(cmd.Args, extra...)
	}
	cmd.Dir = workdir
	addTrustedWorkdirArg(a.Provider, cmd)

	if Base(a.Provider) == "cursor" {
		path := filepath.Join(workdir, "PROMPT.md")
		if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
			return nil, fmt.Errorf("llmagent: write %s: %w", path, err)
		}
		// Cursor reads PROMPT.md on start and we leave the file in place;
		// removing it racing with the subprocess can lose the prompt.
	}
	return cmd, nil
}

func addTrustedWorkdirArg(provider string, cmd *exec.Cmd) {
	switch Base(provider) {
	case "codex":
		insertBeforePromptArg(cmd, "--add-dir", ".")
	case "gemini":
		cmd.Args = append(cmd.Args, "--include-directories", ".")
	}
}

func insertBeforePromptArg(cmd *exec.Cmd, args ...string) {
	if len(args) == 0 {
		return
	}
	if len(cmd.Args) > 1 && cmd.Args[len(cmd.Args)-1] == "-" {
		cmd.Args = append(cmd.Args[:len(cmd.Args)-1], append(args, "-")...)
		return
	}
	cmd.Args = append(cmd.Args, args...)
}

// cmdPipes is the bundle of stdin/stdout/stderr pipes wired onto a command.
type cmdPipes struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

// makePipes wires stdin/stdout/stderr pipes onto cmd.
func makePipes(cmd *exec.Cmd) (cmdPipes, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return cmdPipes{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cmdPipes{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return cmdPipes{}, fmt.Errorf("stderr pipe: %w", err)
	}
	return cmdPipes{stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// feedStdin is the default transport: write the prompt verbatim, close stdin.
// Pi uses feedPi (in pi.go) which speaks JSON-RPC instead.
func (a *Agent) feedStdin(stdin io.WriteCloser, prompt string) {
	defer func() {
		if err := stdin.Close(); err != nil {
			a.logger().Debug("close stdin", slog.Any("error", err))
		}
	}()
	if _, err := io.WriteString(stdin, prompt); err != nil {
		a.logger().Debug("write prompt to stdin",
			slog.String("provider", a.Provider),
			slog.Any("error", err))
	}
}

// streamStderr reads stderr line by line, accumulating up to maxStderrBytes
// into tail (used to enrich error messages on subprocess failure) and
// forwarding non-noise lines to onStderr.
func streamStderr(tail *strings.Builder, stderr io.Reader, onStderr func(string)) <-chan struct{} {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			if tail.Len() < maxStderrBytes {
				tail.WriteString(line)
				tail.WriteByte('\n')
			}
			if IsStderrNoise(line) {
				continue
			}
			if onStderr != nil {
				onStderr(line)
			}
		}
	}()
	return doneCh
}

// streamState holds the live state of the stdout reader, protected by mu.
type streamState struct {
	lastActive time.Time
	sessionID  string
	output     strings.Builder
	mu         sync.Mutex
}

// streamStdout reads stdout into stream and signals done when it closes.
// stream's mu serialises access between this goroutine and Run's poll loop.
//
// When sigs is non-nil, the reader also drives the pi RPC protocol: it
// flags agent_end events so the feeder can advance, and captures the
// sessionFile path from get_state responses for later resumption.
func streamStdout(stream *streamState, stdout io.Reader, onEvent func(string), sigs *piSignals) <-chan struct{} {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			stream.mu.Lock()
			if stream.output.Len() < maxStdoutBytes {
				stream.output.WriteString(line)
				stream.output.WriteByte('\n')
			}
			if sid := extractSessionID(line); sid != "" {
				stream.sessionID = sid
			}
			stream.lastActive = time.Now()
			stream.mu.Unlock()
			if sigs != nil {
				observePiLine(line, sigs)
			}
			if onEvent != nil {
				onEvent(line)
			}
		}
		// If pi exited without ever emitting agent_end, unblock the feeder
		// so it doesn't sit waiting forever for a signal that won't come.
		if sigs != nil {
			sigs.signalAgentEnd()
		}
	}()
	return doneCh
}

func (a *Agent) idleExceeded(stream *streamState) bool {
	if a.IdleTimeout <= 0 {
		return false
	}
	stream.mu.Lock()
	idle := time.Since(stream.lastActive)
	stream.mu.Unlock()
	return idle > a.IdleTimeout
}

// finish reaps the process and assembles the Result or error after the
// stdout reader has drained.
//
// For pi, the session ID prefers the path captured from a get_state
// response, then falls back to the SessionID the caller passed in (which
// is still valid since pi treats it as the path to switch to). For other
// providers, sigs is nil and we use whatever the stream reader extracted.
func (a *Agent) finish(
	cmd *exec.Cmd, start time.Time, stream *streamState,
	stderrTail *strings.Builder, stderrDone <-chan struct{},
	sigs *piSignals, requestedSession string,
) (*Result, error) {
	waitErr := cmd.Wait()
	<-stderrDone
	stream.mu.Lock()
	out, sid := stream.output.String(), stream.sessionID
	stream.mu.Unlock()
	if sigs != nil {
		if captured := sigs.sessionFile(); captured != "" {
			sid = captured
		} else if sid == "" {
			sid = requestedSession
		}
	}
	if waitErr != nil {
		return nil, a.failureError(waitErr, out, stderrTail.String())
	}
	return &Result{Output: out, SessionID: sid, Duration: time.Since(start)}, nil
}

func (a *Agent) failureError(waitErr error, stdout, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if trimmed := strings.TrimSpace(stdout); trimmed != "" {
		if detail != "" {
			detail = detail + "\n" + trimmed
		} else {
			detail = trimmed
		}
	}
	if detail != "" {
		return fmt.Errorf("%s failed: %w:\n%s", a.Provider, waitErr, detail)
	}
	return fmt.Errorf("%s failed: %w", a.Provider, waitErr)
}

// Probe verifies the agent's binary exists and produces output for a trivial
// prompt. It is called automatically by Run on first use; calling Probe
// directly lets callers warm up agents in parallel.
//
// Probe is idempotent: after one success or failure, subsequent calls are
// no-ops returning the same outcome.
func (a *Agent) Probe(ctx context.Context) error {
	a.probeOnce.Do(func() { a.probeErr = a.runProbe(ctx) })
	return a.probeErr
}

// Reset clears the probe outcome so the next Run probes again. Useful after
// the operator confirms a previously-failing binary is now installed.
func (a *Agent) Reset() {
	a.probeOnce = sync.Once{}
	a.probeErr = nil
}

func (a *Agent) runProbe(ctx context.Context) error {
	cmd, err := a.newCmd(ctx)
	if err != nil {
		return fmt.Errorf("probe %s: %w", a.Provider, err)
	}
	a.logger().Info("llmagent probe starting",
		slog.String("provider", a.Provider),
		slog.Any("argv", cmd.Args),
		slog.String("workdir", cmdWorkDir(cmd)),
		slog.String("home", cmdEnvValue(cmd, "HOME")),
		slog.String("xdg_data_home", cmdEnvValue(cmd, "XDG_DATA_HOME")),
		slog.String("xdg_state_home", cmdEnvValue(cmd, "XDG_STATE_HOME")),
		slog.String("xdg_cache_home", cmdEnvValue(cmd, "XDG_CACHE_HOME")),
		slog.String("tmpdir", cmdEnvValue(cmd, "TMPDIR")))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("probe %s stdin: %w", a.Provider, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("probe %s stdout: %w", a.Provider, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("probe %s stderr: %w", a.Provider, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("probe %s start %s: %w", a.Provider, describeCmd(cmd), err)
	}
	a.logger().Info("llmagent probe spawned",
		slog.String("provider", a.Provider),
		slog.Int("pid", cmd.Process.Pid))

	go a.feedStdin(stdin, a.probePrompt())
	stdoutTail := newProbeOutput()
	stderrTail := &strings.Builder{}
	stdoutDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stdoutTail, stdout)
		stdoutDone <- copyErr
	}()
	stderrDone := streamStderr(stderrTail, stderr, nil)

	timeout := a.probeTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-stdoutTail.first:
		killProcess(cmd)
		_ = cmd.Wait() //nolint:errcheck // killed after successful first byte
		<-stdoutDone
		<-stderrDone
		a.logger().Info("llmagent probe ok", slog.String("provider", a.Provider))
		return nil
	case <-stdoutDone:
		waitErr := cmd.Wait()
		<-stderrDone
		if strings.TrimSpace(stdoutTail.String()) != "" {
			a.logger().Info("llmagent probe ok", slog.String("provider", a.Provider))
			return nil
		}
		return a.probeFailureError(cmd, waitErr, stdoutTail.String(), stderrTail.String())
	case <-timer.C:
		killProcess(cmd)
		waitErr := cmd.Wait()
		<-stdoutDone
		<-stderrDone
		return a.probeTimeoutError(cmd, waitErr, timeout, stdoutTail.String(), stderrTail.String())
	case <-ctx.Done():
		killProcess(cmd)
		waitErr := cmd.Wait()
		<-stdoutDone
		<-stderrDone
		return a.probeCancelledError(cmd, waitErr, ctx.Err(), stdoutTail.String(), stderrTail.String())
	}
}

func (a *Agent) probeFailureError(cmd *exec.Cmd, waitErr error, stdout, stderr string) error {
	status := probeExitStatus(waitErr)
	detail := probeOutputDetail(stdout, stderr)
	if detail != "" {
		return fmt.Errorf("probe %s %s %s before producing stdout:%s", a.Provider, describeCmd(cmd), status, detail)
	}
	return fmt.Errorf("probe %s %s %s before producing stdout", a.Provider, describeCmd(cmd), status)
}

func (a *Agent) probeTimeoutError(cmd *exec.Cmd, waitErr error, timeout time.Duration, stdout, stderr string) error {
	detail := probeOutputDetail(stdout, stderr)
	if detail != "" {
		return fmt.Errorf("probe %s %s timed out after %v; killed process (%s):%s",
			a.Provider, describeCmd(cmd), timeout, probeExitStatus(waitErr), detail)
	}
	return fmt.Errorf("probe %s %s timed out after %v; killed process (%s)",
		a.Provider, describeCmd(cmd), timeout, probeExitStatus(waitErr))
}

func (a *Agent) probeCancelledError(cmd *exec.Cmd, waitErr, ctxErr error, stdout, stderr string) error {
	detail := probeOutputDetail(stdout, stderr)
	if detail != "" {
		return fmt.Errorf("probe %s %s cancelled: %w; killed process (%s):%s",
			a.Provider, describeCmd(cmd), ctxErr, probeExitStatus(waitErr), detail)
	}
	return fmt.Errorf("probe %s %s cancelled: %w; killed process (%s)",
		a.Provider, describeCmd(cmd), ctxErr, probeExitStatus(waitErr))
}

// probePrompt is the trivial input written to stdin during Probe. Pi expects
// a JSON command on every line; everything else accepts plain text.
func (a *Agent) probePrompt() string {
	if Base(a.Provider) == "pi" {
		return `{"type":"get_state"}` + "\n"
	}
	return "Respond with OK"
}

func (a *Agent) probeTimeout() time.Duration {
	if a.ProbeTimeout > 0 {
		return a.ProbeTimeout
	}
	if IsLocal(a.Provider) {
		return defaultLocalProbeTimeout
	}
	return defaultProbeTimeout
}

func (a *Agent) newCmd(ctx context.Context) (*exec.Cmd, error) {
	if a.NewCmd != nil {
		return a.NewCmd(ctx, a)
	}
	return DefaultCommand(ctx, a)
}

func (a *Agent) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Agent) withTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, a.Timeout)
}

// processEnv returns os.Environ() with TMPDIR overrides and any additional
// extra entries appended. Duplicate keys are intentional: on Linux and macOS
// the last assignment wins.
func (a *Agent) processEnv(extra ...string) []string {
	env := os.Environ()
	if a.TmpDir != "" {
		env = append(env,
			"TMPDIR="+a.TmpDir,
			"TMP="+a.TmpDir,
			"TEMP="+a.TmpDir,
		)
	}
	env = append(env, a.Env...)
	return append(env, extra...)
}

func describeCmd(cmd *exec.Cmd) string {
	if cmd == nil || len(cmd.Args) == 0 {
		return "<unknown command>"
	}
	quoted := make([]string, len(cmd.Args))
	for i, arg := range cmd.Args {
		quoted[i] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func cmdEnvValue(cmd *exec.Cmd, key string) string {
	prefix := key + "="
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	value := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			value = v
		}
	}
	return value
}

func cmdWorkDir(cmd *exec.Cmd) string {
	if cmd != nil && cmd.Dir != "" {
		return cmd.Dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

type probeOutput struct { //nolint:govet // Keep synchronisation fields adjacent to the buffer they guard.
	first chan struct{}
	once  sync.Once
	mu    sync.Mutex
	buf   strings.Builder
}

func newProbeOutput() *probeOutput {
	return &probeOutput{first: make(chan struct{})}
}

func (p *probeOutput) Write(b []byte) (int, error) {
	n := len(b)
	if len(b) > 0 {
		p.once.Do(func() { close(p.first) })
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf.Len() < maxStderrBytes {
		remaining := maxStderrBytes - p.buf.Len()
		if len(b) > remaining {
			b = b[:remaining]
		}
		p.buf.Write(b)
	}
	return n, nil
}

func (p *probeOutput) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

func probeExitStatus(err error) string {
	if err == nil {
		return "exited with code 0"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return "terminated by signal " + status.Signal().String()
			}
		}
		return fmt.Sprintf("exited with code %d", exitErr.ExitCode())
	}
	return "exited with error: " + err.Error()
}

func probeOutputDetail(stdout, stderr string) string {
	var b strings.Builder
	if out := strings.TrimSpace(stdout); out != "" {
		b.WriteString("\nstdout:\n")
		b.WriteString(out)
	}
	if errOut := strings.TrimSpace(stderr); errOut != "" {
		b.WriteString("\nstderr:\n")
		b.WriteString(errOut)
	}
	return b.String()
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill() //nolint:errcheck // best effort
	}
}

func killAndReap(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// The subprocess may already have exited; either way we just want
		// it gone, so we discard kill/wait errors.
		_ = cmd.Process.Kill() //nolint:errcheck // best effort
	}
	_ = cmd.Wait() //nolint:errcheck // reap zombie, error expected after Kill
}

// extractSessionID parses one stream-json line and returns its sessionID
// or session_id, whichever is present. All providers cyclotron supports
// emit one of these on session-bearing events.
func extractSessionID(line string) string {
	var ev struct {
		SessionID string `json:"sessionID"`
		SID       string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	if ev.SessionID != "" {
		return ev.SessionID
	}
	return ev.SID
}
