//go:build !windows

package reviewer

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunPRCommandStopsTmuxServersTheCommandLeft(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	var out bytes.Buffer
	code, err := RunPRCommand([]string{"sh", "-c",
		`tmux -L left new-session -d -s s "sleep 60" && tmux -L left display-message -p '#{pid}'`}, nil, &out, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v output=%s", code, err, out.String())
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("tmux server %d is still running", pid)
		}
	}
}
