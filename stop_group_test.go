package llmagent

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
