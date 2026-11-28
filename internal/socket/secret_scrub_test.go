//go:build !windows

package socket

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
	"github.com/steveyegge/gastown/internal/tmux"
)

// A remote polecat's pane runs `gt worker-attach`, which authenticates with the
// worker token from its environment.
func TestRemotePolecatPaneKeepsWorkerToken(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	server := fmt.Sprintf("gt-test-worker-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", server, "kill-server").Run() })

	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	stub := "#!/bin/sh\necho \"$1 ${GT_WORKER_TOKEN:+token-present}\" > " + out + ".tmp && mv " + out + ".tmp " + out + "\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"GT_ROLE": "rig/polecats/toast", "GT_ROOT": dir}
	b := &SocketBackend{settings: &Settings{Token: "fixture"}}
	argv, err := b.WrapCommand(execution.Endpoint{ID: "s", Address: "unix:/nowhere"}, []string{"agent"}, env)
	if err != nil {
		t.Fatal(err)
	}
	for i := range argv {
		argv[i] = config.ShellQuote(argv[i])
	}
	command := "PATH=" + config.ShellQuote(dir) + ":$PATH; exec " + strings.Join(argv, " ")
	if err := tmux.NewTmuxWithSocket(server).NewSessionWithCommandAndEnv("gt-remote-polecat", dir, command, env); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(out)
		if err == nil {
			if got, want := strings.TrimSpace(string(data)), "worker-attach token-present"; got != want {
				t.Errorf("pane ran %q, want %q", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker-attach never ran: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
