package llmagent

import (
	"bufio"
	"bytes"
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
	// "claude", "agy:gemini-3.1-pro", "opencode:opencode/deepseek-v4-flash".
	Provider string

	// TmpDir, when non-empty, is exported as TMPDIR/TMP/TEMP to the
	// subprocess so its scratch files land in a known location.
	TmpDir string

	// IncludeDirs are directories the agent is allowed to read or write.
	// Used by providers that take a directory allow-list (agy, codex).
	IncludeDirs []string

	// Env is appended to the subprocess environment after TMPDIR overrides.
	// Each entry must be "KEY=VALUE".
	Env []string

	// ExtraArgs is appended to the provider command line after the built-in
	// flags but before any prompt placeholder. Use this for provider-specific
	// flags the caller wants to set without overriding NewCmd. Defaults stay
	// provider-native; for example, pi thinking/tools/skills are left enabled
	// unless the caller explicitly disables them here.
	ExtraArgs []string

	// Timeout is the wall-clock limit for one Run. Zero means no limit.
	Timeout time.Duration

	// IdleTimeout is the maximum gap between stdout lines. Zero means no
	// limit; the subprocess can stall indefinitely.
	IdleTimeout time.Duration

	// ProbeTimeout overrides the first-byte timeout used by Probe.
	// Zero picks 75s for cloud providers and 5 min for local ones.
	ProbeTimeout time.Duration

	// MaxStdoutBytes caps the captured Result.Output. Zero uses the default
	// (8 MB). A positive value uses exactly that many bytes; a negative value
	// disables the cap (use carefully — a runaway stream can OOM the host).
	//
	// Lines beyond the cap are silently dropped from the captured Output but
	// are still observed by OnEvent and the provider's protocol filter (so
	// pi RPC events still drive feedPi correctly).
	MaxStdoutBytes int

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

	// Logger overrides Agent.Logger for this single Run, so per-call context
	// (e.g. a sample's sha256) attaches to llmagent's lifecycle records
	// (`llmagent invoke`, `llmagent probe ok`, idle/exit warnings). Falls back
	// to Agent.Logger, then slog.Default. Optional.
	Logger *slog.Logger

	// SessionID, if non-empty, asks the provider to resume that session.
	// Providers that don't support resumption (codex) ignore this field.
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

	// Per-call logger (e.g. with a sha256 attr) overrides the agent's; falls
	// back to Agent.Logger via runLogger.
	rlog := a.runLogger(opts.Logger)

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
	rlog.Info("llmagent invoke",
		slog.String("provider", a.Provider),
		slog.Int("pid", cmd.Process.Pid),
		slog.Int("prompt_bytes", len(prompt)),
		slog.String("workdir", workdir))

	stderrTail := &strings.Builder{}
	stderrDone := streamStderr(stderrTail, pipes.stderr, opts.OnStderr)
	stream := &streamState{lastActive: time.Now()}

	var sigs *piSignals
	var capture func(string) bool
	if Base(a.Provider) == "pi" {
		sigs = newPiSignals(pipes.stdin, a.logger())
		capture = piCaptureFilter
		go a.feedPi(ctx, pipes.stdin, prompt, opts.SessionID, sigs)
	} else {
		go a.feedStdin(pipes.stdin, prompt)
	}
	stdoutDone := streamStdout(stream, pipes.stdout, opts.OnEvent, sigs, capture, a.MaxStdoutBytes)

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
// args, sets the workdir, and writes the prompt-file side-channel for the
// providers that read the prompt from disk (see promptFile).
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

	if name := promptFile(a.Provider); name != "" {
		path := filepath.Join(workdir, name)
		if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
			return nil, fmt.Errorf("llmagent: write %s: %w", path, err)
		}
		// The provider reads the file on start and we leave it in place;
		// removing it racing with the subprocess can lose the prompt.
	}
	return cmd, nil
}

func addTrustedWorkdirArg(provider string, cmd *exec.Cmd) {
	if usesAgy(provider) {
		cmd.Args = append(cmd.Args, "--add-dir", ".")
		return
	}
	if Base(provider) == "codex" {
		insertBeforePromptArg(cmd, "--add-dir", ".")
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

// feedStdin is the default transport: write the prompt, close stdin. agy
// frames it as one stream-json message; every other provider takes it
// verbatim. Pi uses feedPi (in pi.go), which speaks JSON-RPC instead.
//
// The providers that already got the prompt as a file get an immediate close
// and nothing else. They do not read stdin at all, so writing to it would fill
// the pipe buffer and park this goroutine until the subprocess exits -- which a
// prompt under 64 KiB hides and a larger one does not.
func (a *Agent) feedStdin(stdin io.WriteCloser, prompt string) {
	defer func() {
		if err := stdin.Close(); err != nil {
			a.logger().Debug("close stdin", slog.Any("error", err))
		}
	}()
	if promptFile(a.Provider) != "" {
		return
	}
	if usesAgy(a.Provider) {
		prompt = agyStreamInput(prompt)
	}
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
//
// captureFilter, when non-nil, decides per line whether to keep it in the
// captured Output. Returning false drops the line from Output (it is still
// observed by sigs/onEvent so protocol semantics aren't affected). pi's
// filter drops thinking_delta events, which are O(n^2) in the prompt's
// thinking length and otherwise blow the cap before any text is emitted.
func streamStdout(
	stream *streamState, stdout io.Reader,
	onEvent func(string), sigs *piSignals,
	captureFilter func(string) bool, capBytes int,
) <-chan struct{} {
	doneCh := make(chan struct{})
	if capBytes == 0 {
		capBytes = maxStdoutBytes
	}
	go func() {
		defer close(doneCh)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			keep := captureFilter == nil || captureFilter(line)
			stream.mu.Lock()
			if keep && (capBytes < 0 || stream.output.Len() < capBytes) {
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
	// A prompt-file provider is handed --prompt-file on its argv and reads
	// stdin not at all, so the probe has to write that file or the process
	// exits before it produces a line: `muse exec` reported "failed to read
	// --prompt-file MUSE_PROMPT.md" on every probe, and the chain blacklisted
	// its own head for twenty minutes a round, on every model, for as long as
	// this was missing. Run writes the file into the caller's workdir; the
	// probe has no workdir of its own, and inheriting the caller's cwd is not
	// an alternative -- it would drop a file into a repository the caller is
	// about to inspect for changes, and hand muse that tree as its workspace
	// root. So the probe gets a temp directory that lives exactly as long as
	// it does. Providers that take the prompt on stdin keep the inherited cwd
	// they have always probed in.
	if name := promptFile(a.Provider); name != "" {
		dir, mkErr := os.MkdirTemp(a.TmpDir, "llmagent-probe-")
		if mkErr != nil {
			return fmt.Errorf("probe %s workdir: %w", a.Provider, mkErr)
		}
		// Every path out of the select below reaps the process first, so the
		// directory outlives the reader of the file in it.
		defer func() {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				a.logger().Debug("remove probe workdir",
					slog.String("provider", a.Provider), slog.Any("error", rmErr))
			}
		}()
		if wErr := os.WriteFile(filepath.Join(dir, name), []byte(a.probePrompt()), 0o600); wErr != nil {
			return fmt.Errorf("probe %s write %s: %w", a.Provider, name, wErr)
		}
		cmd.Dir = dir
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
		// Output started. Give the provider a bounded moment to finish the
		// line so the verdict classifies a whole event rather than a prefix.
		// stdoutDone carries a single buffered value, so note when this drains
		// it -- receiving it twice would block forever.
		stdoutEnded := false
		select {
		case <-stdoutTail.line:
		case <-stdoutDone:
			stdoutEnded = true
		case <-time.After(probeLineGrace):
		}
		killProcess(cmd)
		_ = cmd.Wait() //nolint:errcheck // killed after capturing the first line
		if !stdoutEnded {
			<-stdoutDone
		}
		<-stderrDone
		return a.probeVerdict(cmd, stdoutTail.String(), stderrTail.String())
	case <-stdoutDone:
		waitErr := cmd.Wait()
		<-stderrDone
		if strings.TrimSpace(stdoutTail.String()) != "" {
			return a.probeVerdict(cmd, stdoutTail.String(), stderrTail.String())
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

// probeLineGrace bounds how long Probe waits, after the first byte, for the
// rest of that line. Providers emit their first event in one write, so this
// almost never elapses; it exists so a provider that dribbles a partial line
// cannot stall the probe until its full timeout.
const probeLineGrace = 2 * time.Second

// probeVerdict decides whether probe output actually indicates a working
// provider. Producing bytes is not evidence of health: a provider whose
// credential is missing or whose backend rejects the request launches
// normally, prints one error event, and exits 1. Treating that as success --
// as this probe did until it started reading the line it captured -- makes
// every failover chain lead with a provider that cannot work, and the failure
// only surfaces once per real batch. An opencode credential invisible to its
// service account went undetected this way for three days, logging
// "llmagent probe ok" immediately before each failed invoke.
//
// Quota and transient conditions deliberately pass. They prove the opposite of
// a broken provider -- the binary ran, the credential authenticated, the
// backend answered -- and they clear on their own. Probe results are latched
// for the process lifetime (see Probe), so failing here would retire a
// provider that is merely out of budget this hour; that belongs to the
// caller's cooldown, which already reads these same signals off the real run.
func (a *Agent) probeVerdict(cmd *exec.Cmd, stdout, stderr string) error {
	combined := stdout + "\n" + stderr
	if _, ok := DetectQuota(combined); ok {
		a.logger().Info("llmagent probe ok (quota-limited; deferring to run-time cooldown)",
			slog.String("provider", a.Provider))
		return nil
	}
	if DetectTransient(combined) {
		a.logger().Info("llmagent probe ok (transient backend error)",
			slog.String("provider", a.Provider))
		return nil
	}
	if msg := detectStreamError(stdout); msg != "" {
		return fmt.Errorf("probe %s %s reported an error: %s", a.Provider, describeCmd(cmd), msg)
	}
	a.logger().Info("llmagent probe ok", slog.String("provider", a.Provider))
	return nil
}

// detectStreamError returns the message from the first stream event that
// declares a failure, or "" if none does. Every provider renders one as a JSON
// object with type "error" (opencode, codex, agy) or an is_error flag
// (claude); the message hides one level deeper for opencode, which nests it
// under error.data.
func detectStreamError(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		// Field order is govet's fieldalignment ordering (the bool last), not a
		// grouping anyone chose.
		var ev struct {
			Type  string `json:"type"`
			Error struct {
				Name    string `json:"name"`
				Message string `json:"message"`
				Data    struct {
					Message string `json:"message"`
				} `json:"data"`
			} `json:"error"`
			IsError bool `json:"is_error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Type != "error" && !ev.IsError {
			continue
		}
		switch {
		case ev.Error.Data.Message != "":
			return qualifyErr(ev.Error.Name, ev.Error.Data.Message)
		case ev.Error.Message != "":
			return qualifyErr(ev.Error.Name, ev.Error.Message)
		case ev.Error.Name != "":
			return ev.Error.Name
		}
		return truncateProbeLine(line)
	}
	return ""
}

// qualifyErr prefixes a message with its error name when the provider supplies
// one; claude reports a bare message, where opencode names the class.
func qualifyErr(name, msg string) string {
	if name == "" {
		return msg
	}
	return name + ": " + msg
}

// truncateProbeLine bounds an unrecognised error event quoted into an error
// message, so a large payload cannot dominate the log line.
func truncateProbeLine(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
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

// runLogger picks the per-call logger if RunOptions.Logger is set, otherwise
// falls back to the agent's logger. Lets callers attach per-invocation
// context (sha256, worker id) to llmagent's lifecycle records.
func (a *Agent) runLogger(perCall *slog.Logger) *slog.Logger {
	if perCall != nil {
		return perCall
	}
	return a.logger()
}

func (a *Agent) withTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, a.Timeout)
}

// processEnv returns os.Environ() with TMPDIR overrides and Agent.Env
// appended. Duplicate keys are intentional: on Linux and macOS the last
// assignment wins.
func (a *Agent) processEnv() []string {
	env := os.Environ()
	if a.TmpDir != "" {
		env = append(env,
			"TMPDIR="+a.TmpDir,
			"TMP="+a.TmpDir,
			"TEMP="+a.TmpDir,
		)
	}
	return append(env, a.Env...)
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
	first    chan struct{}
	line     chan struct{}
	once     sync.Once
	lineOnce sync.Once
	mu       sync.Mutex
	buf      strings.Builder
}

func newProbeOutput() *probeOutput {
	return &probeOutput{first: make(chan struct{}), line: make(chan struct{})}
}

func (p *probeOutput) Write(b []byte) (int, error) {
	n := len(b)
	if len(b) > 0 {
		p.once.Do(func() { close(p.first) })
	}
	// A complete line is the unit the verdict can classify: every provider
	// emits its stream as one JSON object per line, so a whole line is either
	// a usable event or an error report, where a first-byte prefix is neither.
	if bytes.IndexByte(b, '\n') >= 0 {
		p.lineOnce.Do(func() { close(p.line) })
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

// extractSessionID parses one stream-json line and returns its sessionID,
// session_id, conversation_id (agy), or the id of the session stream the record
// belongs to (muse), whichever is present. All providers cyclotron supports emit
// one of these on session-bearing events.
//
// muse's is nested because its output is an event-sourced record log rather than
// a message stream: every line is an envelope naming the stream it belongs to,
// and the session's id is that stream's id. The envelope is decoded as raw JSON
// so a provider that happens to put something else under "stream" cannot fail
// the whole line's decode and silently cost every provider its session.
func extractSessionID(line string) string {
	var ev struct {
		SessionID      string          `json:"sessionID"`
		SID            string          `json:"session_id"`
		ConversationID string          `json:"conversation_id"`
		Stream         json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	switch {
	case ev.SessionID != "":
		return ev.SessionID
	case ev.SID != "":
		return ev.SID
	case ev.ConversationID != "":
		return ev.ConversationID
	}
	var stream struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(ev.Stream, &stream); err == nil && stream.Kind == "session" {
		return stream.ID
	}
	return ""
}
