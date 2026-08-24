package llmagent

import (
	"context"
	"fmt"
	"os/exec"
)

// DefaultCommand builds the *exec.Cmd that invokes provider with the agent's
// IncludeDirs, Model, TmpDir, Env, and ExtraArgs. It supports the providers
// cyclotron uses today: claude, agy (alias: gemini), codex, opencode, pi,
// cursor.
//
// The returned command:
//   - reads its prompt from stdin (agy as one stream-json message; cursor
//     reads PROMPT.md, written by Run);
//   - has TmpDir injected as TMPDIR/TMP/TEMP via Env;
//   - has any Agent.ExtraArgs appended after the built-in flags (and before
//     a trailing positional, e.g. codex's `-` or cursor's prompt argument);
//   - has stdin/stdout/stderr left for the caller to wire pipes onto.
//
// To support a new provider, write a custom NewCmd on Agent.
func DefaultCommand(ctx context.Context, agent *Agent) (*exec.Cmd, error) {
	base, model, effort := Base(agent.Provider), Model(agent.Provider), Effort(agent.Provider)
	switch base {
	case "claude":
		args := []string{
			"--verbose",
			"--output-format", "stream-json",
			"--dangerously-skip-permissions",
		}
		// Accepts an alias ("opus", "sonnet", "fable") or a full model name.
		if model != "" {
			args = append(args, "--model", model)
		}
		if effort != "" {
			args = append(args, "--effort", effort)
		}
		args = append(args, agent.ExtraArgs...)
		cmd := exec.CommandContext(ctx, "claude", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "agy", "gemini":
		// --input-format stream-json implies print mode and takes the prompt
		// from stdin; see agy.go for why the prompt does not go in argv.
		args := []string{
			"--output-format", "stream-json",
			"--input-format", "stream-json",
			"--dangerously-skip-permissions",
		}
		// Model names carry their own effort tier ("gemini-3.1-pro-high"), but
		// the base name plus --effort resolves too; agy exits non-zero and
		// lists the alternatives when neither does.
		if model != "" {
			args = append(args, "--model", model)
		}
		if effort != "" {
			args = append(args, "--effort", effort)
		}
		for _, d := range agent.IncludeDirs {
			args = append(args, "--add-dir", d)
		}
		args = append(args, agent.ExtraArgs...)
		cmd := exec.CommandContext(ctx, "agy", args...)
		// agy runs unsandboxed unless asked (--sandbox), so unlike the gemini
		// CLI it needs no environment override to write outside its worktree.
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "codex":
		// --full-auto and --sandbox are mutually exclusive in codex >=0.130:
		// --full-auto is a deprecated alias for --sandbox workspace-write, so
		// passing both leaves codex enforcing a sandbox the caller asked it to
		// drop. On Linux that sandbox wraps every shell call in bwrap, which
		// cannot create a user namespace under a systemd unit with
		// RestrictNamespaces=true -- the agent then reports
		// "bwrap: No permissions to create a new namespace" and returns having
		// changed nothing. On macOS it refuses shell writes to --add-dir paths
		// with EPERM. --dangerously-bypass-approvals-and-sandbox expresses the
		// intent in one flag. Callers that want codex sandboxed should say so
		// through a custom Agent.NewCmd; the isolation that matters for a
		// service belongs to its unit, not to the agent it spawns.
		args := []string{
			"exec", "--json",
			"--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox",
		}
		for _, d := range agent.IncludeDirs {
			args = append(args, "--add-dir", d)
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		// codex exposes no --effort flag; reasoning effort is a config key, the
		// same one ~/.codex/config.toml sets globally. Passing it with -c
		// overrides that per invocation.
		if effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+effort)
		}
		args = append(args, agent.ExtraArgs...)
		args = append(args, "-")
		cmd := exec.CommandContext(ctx, "codex", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "opencode":
		// `run` is non-interactive, but tool permissions still require explicit
		// approval. --auto grants those permissions for unattended queue workers.
		// OpenCode calls its model-specific reasoning setting a variant.
		args := []string{"run", "--format", "json", "--auto"}
		if model != "" {
			args = append(args, "--model", model)
		}
		if effort != "" {
			args = append(args, "--variant", effort)
		}
		args = append(args, agent.ExtraArgs...)
		cmd := exec.CommandContext(ctx, "opencode", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "pi":
		// pi --mode rpc speaks a JSON-over-stdio RPC protocol; Run drives
		// it with an optional switch_session, the prompt, and a closing
		// get_state to capture the session file. See docs/rpc.md upstream:
		// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/rpc.md
		args := []string{"--mode", "rpc"}
		if model != "" {
			args = append(args, "--model", model)
		}
		args = append(args, agent.ExtraArgs...)
		cmd := exec.CommandContext(ctx, "pi", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "cursor":
		// Cursor's agent binary takes the prompt as a positional argv;
		// Run writes PROMPT.md into the workdir.
		m := model
		if m == "" {
			m = "auto"
		}
		args := []string{
			"-p", "--force",
			"--output-format", "stream-json",
			"--stream-partial-output",
			"--model", m,
		}
		args = append(args, agent.ExtraArgs...)
		args = append(args, "Follow the instructions in PROMPT.md exactly")
		cmd := exec.CommandContext(ctx, "agent", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil
	}
	return nil, fmt.Errorf("llmagent: unknown provider %q", agent.Provider)
}

// resumeArgs returns CLI flags to resume sessionID for an existing provider,
// or nil if the provider does not support explicit session IDs. codex is
// excluded because it emits no resumable session ID.
func resumeArgs(provider, sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	if usesAgy(provider) {
		// agy conversation IDs are UUIDs, unlike the old gemini CLI's session
		// indexes, which collided across workers sharing a workdir.
		return []string{"--conversation", sessionID}
	}
	switch Base(provider) {
	case "claude":
		return []string{"-r", sessionID}
	case "opencode":
		return []string{"-s", sessionID}
	case "cursor":
		return []string{"--resume", sessionID}
	}
	return nil
}
