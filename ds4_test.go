package llmagent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func installDS4Stub(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ds4-agent")
	// Resolve runtime assets before --chdir, just as the native binary does.
	const script = `#!/bin/sh
[ -f runtime-asset ] || { echo 'missing runtime assets' >&2; exit 1; }
[ "$1" = --non-interactive ] || exit 2
shift
while [ "$#" -gt 0 ]; do
  case "$1" in
    --chdir) cd "$2" || exit 3; shift 2 ;;
    *) echo "unexpected flag: $1" >&2; exit 4 ;;
  esac
done
prompt=$(/bin/cat; printf .)
prompt=${prompt%.}
workspace=$(/bin/pwd -P)
if [ "$prompt" = 'Respond with OK' ]; then
  printf '%s' "$workspace" > "$PROBE_WORKSPACE_LOG"
  printf 'OK\n'
else
  printf '%s\nworkspace:%s\n' "$prompt" "$workspace"
fi
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime-asset"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDS4ProbeAndRun(t *testing.T) {
	runtimeDir := t.TempDir()
	binary := installDS4Stub(t, runtimeDir)
	// A symlink on PATH must still resolve runtime assets in the checkout.
	binDir := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(binDir, "ds4-agent")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	probeLog := filepath.Join(t.TempDir(), "probe-workspace")
	workdir := t.TempDir()
	agent := &Agent{
		Provider: "ds4", TmpDir: t.TempDir(),
		Env:          []string{"PROBE_WORKSPACE_LOG=" + probeLog},
		ProbeTimeout: 5 * time.Second, Timeout: 5 * time.Second,
	}
	// Larger than a pipe buffer, with multiple lines and shell metacharacters.
	prompt := strings.Repeat("line one\n`literal` $(literal) 'quoted'\n", 4096)
	var events []string
	result, err := agent.Run(context.Background(), prompt, workdir, RunOptions{
		OnEvent:   func(line string) { events = append(events, line) },
		SessionID: "unsupported-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatal(err)
	}
	want := prompt + "\nworkspace:" + resolvedWorkdir + "\n"
	if result.Output != want {
		t.Fatalf("output did not preserve prompt and workspace: got %d bytes, want %d", len(result.Output), len(want))
	}
	if result.SessionID != "" || len(events) == 0 {
		t.Fatalf("SessionID = %q, events = %d", result.SessionID, len(events))
	}
	probeWorkspace, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(probeWorkspace), "llmagent-probe-") || string(probeWorkspace) == resolvedWorkdir {
		t.Fatalf("probe used unexpected workspace %q", probeWorkspace)
	}
	if _, err := os.Stat(string(probeWorkspace)); !os.IsNotExist(err) {
		t.Fatalf("probe workspace was not removed: %v", err)
	}
}

func TestDS4CommandOptions(t *testing.T) {
	dir := t.TempDir()
	installDS4Stub(t, dir)
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		spec string
		args []string
	}{
		{"ds4", nil},
		{"ds4-agent:/models/a b.gguf@75", []string{"--model", "/models/a b.gguf", "--think-level", "75"}},
		{"ds4@high", []string{"--think"}},
		{"ds4@max", []string{"--think-max"}},
		{"ds4@none", []string{"--nothink"}},
		{"ds4@0", []string{"--think-level", "0"}},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			agent := &Agent{Provider: tc.spec, ExtraArgs: []string{"--ctx", "8192"}}
			cmd, err := DefaultCommand(context.Background(), agent)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{"--non-interactive"}, tc.args...)
			want = append(want, "--ctx", "8192")
			if !slices.Equal(cmd.Args[1:], want) {
				t.Fatalf("args = %v, want %v", cmd.Args[1:], want)
			}
			if !IsLocal(tc.spec) || agent.probeTimeout() != defaultLocalProbeTimeout {
				t.Fatal("ds4 must use local provider defaults")
			}
		})
	}
}

func TestDS4SiblingFallback(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "ds4")
	callerDir := filepath.Join(root, "caller")
	for _, dir := range []string{runtimeDir, callerDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installDS4Stub(t, runtimeDir)
	t.Setenv("PATH", t.TempDir())
	t.Chdir(callerDir)
	cmd, err := DefaultCommand(context.Background(), &Agent{Provider: "ds4"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != want || cmd.Path != filepath.Join(want, "ds4-agent") {
		t.Fatalf("unexpected sibling command: path=%q dir=%q", cmd.Path, cmd.Dir)
	}
}
