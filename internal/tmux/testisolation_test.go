package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmuxguard"
)

func TestTestBinaryIsCutOffFromTheRunningServer(t *testing.T) {
	for _, name := range []string{"TMUX", "TMUX_PANE"} {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("%s is still set", name)
		}
	}
	if dir := os.Getenv("TMUX_TMPDIR"); !strings.Contains(filepath.Base(dir), "gt-test-tmux-") {
		t.Error("TMUX_TMPDIR is not a private directory")
	}
}

func TestTmuxCommandsRefuseTheTownSocket(t *testing.T) {
	const probe = "gt-guard-probe"
	t.Setenv("GT_TOWN_SOCKET", probe)
	tmuxguard.Isolate() // adds the probe to the refused sockets
	previous := GetDefaultSocket()
	SetDefaultSocket(probe)
	t.Cleanup(func() { SetDefaultSocket(previous) })

	for name, call := range map[string]func(){
		"Tmux.run":     func() { _, _ = NewTmuxWithSocket(probe).run("ls") },
		"BuildCommand": func() { BuildCommand("ls") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s allowed the town socket", name)
				}
			}()
			call()
		}()
	}
}
