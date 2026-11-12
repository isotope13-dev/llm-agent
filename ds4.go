package llmagent

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

func usesDS4(provider string) bool {
	base := Base(provider)
	return base == "ds4" || base == "ds4-agent"
}

// ds4Command launches from the native checkout so the default GGUF and Metal
// sources resolve. Prefer PATH, falling back to the sibling experimental build.
func ds4Command(ctx context.Context, agent *Agent) (*exec.Cmd, error) {
	path, err := exec.LookPath("ds4-agent")
	if err != nil {
		path, err = exec.LookPath(filepath.Join("..", "ds4", "ds4-agent"))
		if err != nil {
			return nil, fmt.Errorf("find ds4-agent on PATH or in ../ds4: %w", err)
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve ds4-agent path: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve ds4-agent symlink: %w", err)
	}
	args := []string{"--non-interactive"}
	if model := Model(agent.Provider); model != "" {
		args = append(args, "--model", model)
	}
	switch effort := Effort(agent.Provider); effort {
	case "":
	case "high":
		args = append(args, "--think")
	case "max":
		args = append(args, "--think-max")
	case "none":
		args = append(args, "--nothink")
	default:
		args = append(args, "--think-level", effort)
	}
	args = append(args, agent.ExtraArgs...)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.Env = agent.processEnv()
	return cmd, nil
}

// setCmdWorkdir preserves DS4's runtime directory while moving its tools to the
// requested workspace. Other providers launch directly in the workspace.
func setCmdWorkdir(provider string, cmd *exec.Cmd, workdir string) error {
	if usesDS4(provider) {
		workspace, err := filepath.Abs(workdir)
		if err != nil {
			return fmt.Errorf("resolve ds4 workspace: %w", err)
		}
		cmd.Args = append(cmd.Args, "--chdir", workspace)
		return nil
	}
	cmd.Dir = workdir
	return nil
}
