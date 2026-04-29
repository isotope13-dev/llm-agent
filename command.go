package llmagent

import (
	"context"
	"fmt"
	"os/exec"
)

// DefaultCommand builds the *exec.Cmd that invokes provider with the agent's
// IncludeDirs, Model, TmpDir, and Env. It supports the providers cyclotron
// uses today: claude, gemini, codex, opencode, pi, cursor.
//
// The returned command:
//   - reads its prompt from stdin (cursor reads PROMPT.md, written by Run);
//   - has TmpDir injected as TMPDIR/TMP/TEMP via Env;
//   - has stdin/stdout/stderr left for the caller to wire pipes onto.
//
// To support a new provider, write a custom NewCmd on Agent.
func DefaultCommand(ctx context.Context, agent *Agent) (*exec.Cmd, error) {
	base, model := Base(agent.Provider), Model(agent.Provider)
	switch base {
	case "claude":
		cmd := exec.CommandContext(ctx, "claude",
			"--verbose",
			"--output-format", "stream-json",
			"--dangerously-skip-permissions",
		)
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "gemini":
		args := []string{"--yolo", "--output-format", "stream-json"}
		if model != "" {
			args = append(args, "--model", model)
		}
		for _, d := range agent.IncludeDirs {
			args = append(args, "--include-directories", d)
		}
		cmd := exec.CommandContext(ctx, "gemini", args...)
		// Gemini sandboxes by default; disable so the agent can write to
		// directories outside its working tree.
		cmd.Env = agent.processEnv("GEMINI_SANDBOX=false")
		return cmd, nil

	case "codex":
		args := []string{"exec", "--json", "--full-auto", "--sandbox", "danger-full-access"}
		for _, d := range agent.IncludeDirs {
			args = append(args, "--add-dir", d)
		}
		args = append(args, "-")
		cmd := exec.CommandContext(ctx, "codex", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil

	case "opencode":
		args := []string{"run", "--format", "json"}
		if model != "" {
			args = append(args, "--model", model)
		}
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
			"Follow the instructions in PROMPT.md exactly",
		}
		cmd := exec.CommandContext(ctx, "agent", args...)
		cmd.Env = agent.processEnv()
		return cmd, nil
	}
	return nil, fmt.Errorf("llmagent: unknown provider %q", agent.Provider)
}

// resumeArgs returns CLI flags to resume sessionID for an existing provider,
// or nil if the provider does not support explicit session IDs.
//
// Notes:
//   - gemini uses session indexes (not IDs) that collide across sibling
//     workers sharing a workdir, so it is intentionally excluded;
//   - codex emits no resumable session ID.
func resumeArgs(provider, sessionID string) []string {
	if sessionID == "" {
		return nil
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
