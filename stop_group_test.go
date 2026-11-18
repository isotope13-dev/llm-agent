package llmagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise the actual probe setup, including a child that ignores SIGTERM.
func TestProbeStopsLauncherChildren(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	a := &Agent{Provider: "mock", NewCmd: func(ctx context.Context, _ *Agent) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; sleep 300 >/dev/null 2>&1 & echo $! > "$1"; echo ok; wait`, "sh", pidfile), nil
	}}
	if err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("probe child %d survived", child)
}

// A group leader's backgrounded child must die with stopProcess, not linger as
// an orphan that outlives the probe's workdir.
func TestStopProcessKillsGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatal(err)
	}
	_ = stopProcess(cmd)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	t.Fatalf("child %d survived stopProcess", child)
}
