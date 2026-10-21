package tmux

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// Captured from Codex CLI 0.163.0 starting in an untrusted Gas Town worktree.
const codexTrustDialog = `  Folder access
  /Users/agent/gt/heartworks_graphql_api/reviewer/rig
  Note: You’re in a subdirectory of a Git project. Trusting will apply to the
  repository root:
  /Users/agent/gt/heartworks_graphql_api
  Trust this folder? Codex can read, edit, and run files here, subject to your
  permission settings. Folder settings can run code automatically, even
  without a model request. Continue only if you trust these files. Your trust
  decision will be saved.
› 1. Trust and continue
  2. Quit
  enter continue · esc quit`

func TestCodexTrustDialogRecognized(t *testing.T) {
	if !containsWorkspaceTrustDialog(codexTrustDialog) {
		t.Fatal("Codex 0.163 trust dialog not recognized")
	}
	if name, blocked := containsBlockingStartupDialog(codexTrustDialog); !blocked || name != "workspace trust prompt" {
		t.Errorf("blocked = %v (%q), want the workspace trust prompt", blocked, name)
	}
	opts, err := parseDialogOptions(codexTrustDialog, workspaceTrustMarkers...)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := planDialogSelection(opts)
	if err != nil {
		t.Fatalf("options %v: %v", opts, err)
	}
	if len(keys) != 0 || !opts[0].selected || opts[0].text != "Trust and continue" {
		t.Errorf("keys %v options %v; want Enter on the pre-selected Trust and continue", keys, opts)
	}
	// Once the user (or Gas Town) accepts, the composer below the old dialog
	// means it is gone.
	if _, blocked := containsBlockingStartupDialog(codexTrustDialog + "\n› "); blocked {
		t.Error("a prompt after the dialog still counts as blocked")
	}
}

// The dialog's highlighted option starts with Codex's ready prefix "› ";
// WaitForRuntimeReady must not take it for the prompt.
func TestWaitForRuntimeReady_NotFooledByCodexTrustDialog(t *testing.T) {
	tm := newTestTmux(t)
	rc := &config.RuntimeConfig{Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "› "}}
	show := func(name, text string) {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "pane-*.txt")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(text)
		f.Close()
		if err := tm.NewSessionWithCommand(name, ".", fmt.Sprintf("cat %s; sleep 30", f.Name())); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tm.KillSession(name) })
	}

	dialog := fmt.Sprintf("gt-test-codex-trust-%d", os.Getpid())
	show(dialog, codexTrustDialog+"\n")
	if err := tm.WaitForRuntimeReady(dialog, rc, 1500*time.Millisecond); err == nil {
		t.Error("runtime reported ready while the trust dialog was on screen")
	}

	ready := fmt.Sprintf("gt-test-codex-ready-%d", os.Getpid())
	show(ready, strings.Join([]string{"  Folder access done", "› ", "  GPT-6.1-Sol default · ~/gt/mayor"}, "\n")+"\n")
	if err := tm.WaitForRuntimeReady(ready, rc, 5*time.Second); err != nil {
		t.Errorf("runtime not ready at the Codex prompt: %v", err)
	}
}
