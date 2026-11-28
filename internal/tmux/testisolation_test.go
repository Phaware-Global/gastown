package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestBinaryIsCutOffFromTheRunningServer(t *testing.T) {
	for _, name := range []string{"TMUX", "TMUX_PANE"} {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("%s is still set", name)
		}
	}
	if dir := os.Getenv("TMUX_TMPDIR"); !strings.Contains(filepath.Base(dir), "gt-tmux-") {
		t.Error("TMUX_TMPDIR is not a private directory")
	}
}
