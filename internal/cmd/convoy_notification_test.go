package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recordConvoyGT swaps convoyGT for a fake that records each call as the
// space-joined argument list and never execs anything.
func recordConvoyGT(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	old := convoyGT
	convoyGT = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { convoyGT = old })
	return &calls
}

func TestNotifyConvoyCompletion_StampsAndSkipsDuplicate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	statePath := filepath.Join(binDir, "notified.state")
	bdPath := filepath.Join(binDir, "bd")

	bdScript := `#!/bin/sh
STATE="` + statePath + `"
if [ "$1" = "--allow-stale" ]; then
  shift
fi
case "$1" in
  version)
    exit 0
    ;;
  show)
    if [ -f "$STATE" ]; then
      printf '%s\n' '[{"id":"hq-cv-dup","description":"Owner: mayor/\ncompletion_notified_at: 2026-05-25T02:30:00Z","created_at":"2026-05-25T02:00:00Z"}]'
    else
      printf '%s\n' '[{"id":"hq-cv-dup","description":"Owner: mayor/","created_at":"2026-05-25T02:00:00Z"}]'
    fi
    exit 0
    ;;
  update)
    touch "$STATE"
    exit 0
    ;;
  sql)
    printf '%s\n' '[]'
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(bdPath, []byte(bdScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	sent := recordConvoyGT(t)

	notifyConvoyCompletion(townRoot, "hq-cv-dup", "Duplicate Guard")
	notifyConvoyCompletion(townRoot, "hq-cv-dup", "Duplicate Guard")

	mails := 0
	for _, call := range *sent {
		if strings.HasPrefix(call, "mail send ") {
			mails++
		}
	}
	if mails != 1 {
		t.Fatalf("mail sends = %d, want 1; calls: %q", mails, *sent)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("completion notification state was not recorded: %v", err)
	}
}
