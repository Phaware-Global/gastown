package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeDialogScript renders an option dialog in the pane, highlights option
// <selected> with a "❯" marker, and moves the highlight on Up/Down. Options are
// numbered unless <numbered> is 0, in which case the layout mirrors Claude Code
// 2.1.299: unnumbered options with body prose above and a footer below.
// Enter prints "CHOSEN: <option text>" and exits. It stands in for a real TUI
// so the tests observe which option a key sequence actually lands on.
const fakeDialogScript = `#!/bin/bash
title="$1"; selected="$2"; numbered="$3"; shift 3
options=("$@")
draw() {
  printf '\033[2J\033[H%s\n\n' "$title"
  [ "$numbered" -eq 0 ] && printf " Claude Code'll be able to read, edit, and execute files here.\n\n"
  for i in "${!options[@]}"; do
    if [ "$i" -eq "$selected" ]; then mark='❯'; else mark=' '; fi
    if [ "$numbered" -eq 1 ]; then
      printf ' %s %d. %s\n' "$mark" "$((i+1))" "${options[$i]}"
    else
      printf ' %s %s\n' "$mark" "${options[$i]}"
    fi
  done
  [ "$numbered" -eq 0 ] && printf '\n Enter to confirm · Esc to cancel\n'
}
draw
while IFS= read -rsn1 key; do
  if [ -z "$key" ]; then
    printf '\nCHOSEN: %s\n' "${options[$selected]}"
    exec sleep 30
  fi
  if [ "$key" = $'\033' ]; then
    read -rsn2 rest
    case "$rest" in
      '[B') [ "$selected" -lt $((${#options[@]}-1)) ] && selected=$((selected+1)) ;;
      '[A') [ "$selected" -gt 0 ] && selected=$((selected-1)) ;;
    esac
    draw
  fi
done
`

var invalidSessionChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// startFakeDialog starts a session running the fake numbered dialog and
// returns once the dialog is on screen.
func startFakeDialog(t *testing.T, tm *Tmux, title string, selected int, options ...string) string {
	t.Helper()
	return startFakeDialogMode(t, tm, true, title, selected, options...)
}

// startFakeUnnumberedDialog is startFakeDialog with the Claude Code 2.1.299
// unnumbered layout.
func startFakeUnnumberedDialog(t *testing.T, tm *Tmux, title string, selected int, options ...string) string {
	t.Helper()
	return startFakeDialogMode(t, tm, false, title, selected, options...)
}

func startFakeDialogMode(t *testing.T, tm *Tmux, numbered bool, title string, selected int, options ...string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "dialog.sh")
	if err := os.WriteFile(script, []byte(fakeDialogScript), 0o755); err != nil {
		t.Fatal(err)
	}
	session := "gt-test-fakedlg-" + invalidSessionChars.ReplaceAllString(t.Name(), "-")
	_ = tm.KillSession(session)

	numberedFlag := 0
	if numbered {
		numberedFlag = 1
	}
	cmd := fmt.Sprintf("bash %q %q %d %d", script, title, selected, numberedFlag)
	for _, o := range options {
		cmd += fmt.Sprintf(" %q", o)
	}
	if err := tm.NewSessionWithCommand(session, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })

	waitForPane(t, tm, session, options[len(options)-1], 5*time.Second)
	return session
}

func waitForPane(t *testing.T, tm *Tmux, session, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var content string
	for time.Now().Before(deadline) {
		c, err := tm.CapturePane(session, 40)
		if err == nil {
			content = c
			if strings.Contains(c, want) {
				return c
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane never showed %q; last content:\n%s", want, content)
	return ""
}

func chosen(t *testing.T, tm *Tmux, session string) string {
	t.Helper()
	content := waitForPane(t, tm, session, "CHOSEN:", 5*time.Second)
	for _, line := range strings.Split(content, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "CHOSEN:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no CHOSEN line in:\n%s", content)
	return ""
}

const (
	claudeTrustTitle = "Quick safety check: Is this a project you created or one you trust?"
	trustYes         = "Yes, I trust this folder"
	trustNo          = "No, exit"
)

// dialogLayouts are the renderings every dialog test must handle.
var dialogLayouts = []struct {
	name  string
	start func(t *testing.T, tm *Tmux, title string, selected int, options ...string) string
}{
	{"numbered", startFakeDialog},
	{"unnumbered", startFakeUnnumberedDialog},
}

// Claude Code 2.1.299 lists "No, exit" first and pre-selects it; earlier
// versions listed "Yes" first. Both orders must land on "Yes".
func TestAcceptWorkspaceTrustDialog_SelectsYesInEitherOrder(t *testing.T) {
	cases := []struct {
		name     string
		selected int
		options  []string
	}{
		{"yes first (pre-2.1.299)", 0, []string{trustYes, trustNo}},
		{"no first, no preselected (2.1.299)", 0, []string{trustNo, trustYes}},
		{"yes first, no preselected", 1, []string{trustYes, trustNo}},
		{"no first, yes preselected", 1, []string{trustNo, trustYes}},
	}
	for _, layout := range dialogLayouts {
		for _, tc := range cases {
			t.Run(layout.name+"/"+tc.name, func(t *testing.T) {
				tm := newTestTmux(t)
				session := layout.start(t, tm, claudeTrustTitle, tc.selected, tc.options...)

				if err := tm.AcceptWorkspaceTrustDialog(session); err != nil {
					t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
				}
				if got := chosen(t, tm, session); got != trustYes {
					t.Errorf("chose %q, want %q", got, trustYes)
				}
			})
		}
	}
}

func TestAcceptWorkspaceTrustDialog_CodexOrders(t *testing.T) {
	const title = "> You are in /tmp/demo\nDo you trust the contents of this directory?"
	cases := []struct {
		name     string
		selected int
		options  []string
	}{
		{"yes first", 0, []string{"Yes, continue", "No, quit"}},
		{"no first preselected", 0, []string{"No, quit", "Yes, continue"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := newTestTmux(t)
			session := startFakeDialog(t, tm, title, tc.selected, tc.options...)

			if err := tm.AcceptWorkspaceTrustDialog(session); err != nil {
				t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
			}
			if got := chosen(t, tm, session); got != "Yes, continue" {
				t.Errorf("chose %q, want %q", got, "Yes, continue")
			}
		})
	}
}

// DismissStartupDialogsBlind is the witness's stalled-startup remediation; it
// must not press Enter on whatever happens to be highlighted.
func TestDismissStartupDialogsBlind_SelectsYesOnTrustDialog(t *testing.T) {
	for _, layout := range dialogLayouts {
		for _, options := range [][]string{{trustYes, trustNo}, {trustNo, trustYes}} {
			t.Run(layout.name+"/"+options[0], func(t *testing.T) {
				tm := newTestTmux(t)
				session := layout.start(t, tm, claudeTrustTitle, 0, options...)

				if err := tm.DismissStartupDialogsBlind(session); err != nil {
					t.Fatalf("DismissStartupDialogsBlind: %v", err)
				}
				if got := chosen(t, tm, session); got != trustYes {
					t.Errorf("chose %q, want %q", got, trustYes)
				}
			})
		}
	}
}

func TestAcceptBypassPermissionsWarning_SelectsAcceptInEitherOrder(t *testing.T) {
	const (
		title  = "WARNING: Claude Code running in Bypass Permissions mode"
		accept = "Yes, I accept"
		exit   = "No, exit"
	)
	for _, layout := range dialogLayouts {
		for _, options := range [][]string{{exit, accept}, {accept, exit}} {
			t.Run(layout.name+"/"+options[0], func(t *testing.T) {
				tm := newTestTmux(t)
				session := layout.start(t, tm, title, 0, options...)

				if err := tm.AcceptBypassPermissionsWarning(session); err != nil {
					t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
				}
				if got := chosen(t, tm, session); got != accept {
					t.Errorf("chose %q, want %q", got, accept)
				}
			})
		}
	}
}

// A trust dialog whose options we can't identify must fail loudly rather than
// guess at a key.
func TestAcceptWorkspaceTrustDialog_FailsLoudlyWithoutKnownOption(t *testing.T) {
	for _, layout := range dialogLayouts {
		t.Run(layout.name, func(t *testing.T) {
			tm := newTestTmux(t)
			session := layout.start(t, tm, claudeTrustTitle, 0, "Proceed", "Abort")

			err := tm.AcceptWorkspaceTrustDialog(session)
			if err == nil {
				t.Fatal("expected error when no trust option is recognisable, got nil")
			}
			// Nothing may have been sent: the dialog must still be waiting.
			time.Sleep(300 * time.Millisecond)
			c, _ := tm.CapturePane(session, 40)
			if strings.Contains(c, "CHOSEN:") {
				t.Errorf("a key was sent despite the unrecognised dialog:\n%s", c)
			}
		})
	}
}

// An unnumbered, unhighlighted option that is not led by Yes/No is invisible to
// the parser, which would skew the arrow count and land Enter on it. The
// dialog must be refused with nothing pressed.
func TestAcceptWorkspaceTrustDialog_RefusesUnclassifiableOptionBetweenOptions(t *testing.T) {
	tm := newTestTmux(t)
	session := startFakeUnnumberedDialog(t, tm, claudeTrustTitle, 0, trustNo, "Maybe later", trustYes)

	err := tm.AcceptWorkspaceTrustDialog(session)
	if err == nil {
		t.Fatal("expected error for a dialog line that cannot be classified, got nil")
	}
	if !strings.Contains(err.Error(), "Maybe later") {
		t.Errorf("error should name the offending dialog text, got: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	c, _ := tm.CapturePane(session, 40)
	if strings.Contains(c, "CHOSEN:") {
		t.Errorf("a key was sent despite the unclassifiable option:\n%s", c)
	}
}

// A dialog with no affirmative option at all must press nothing, whichever
// option is highlighted.
func TestAcceptWorkspaceTrustDialog_RefusesDialogWithoutYes(t *testing.T) {
	for _, layout := range dialogLayouts {
		t.Run(layout.name, func(t *testing.T) {
			tm := newTestTmux(t)
			session := layout.start(t, tm, claudeTrustTitle, 0, trustNo, "Cancel")

			if err := tm.AcceptWorkspaceTrustDialog(session); err == nil {
				t.Fatal("expected error for a dialog with no Yes option, got nil")
			}
			time.Sleep(300 * time.Millisecond)
			c, _ := tm.CapturePane(session, 40)
			if strings.Contains(c, "CHOSEN:") {
				t.Errorf("a key was sent to a dialog with no Yes option:\n%s", c)
			}
		})
	}
}
