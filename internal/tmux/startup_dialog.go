package tmux

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Startup dialogs (workspace trust, bypass permissions) are option lists whose
// order and pre-selected entry are chosen by the agent vendor and have changed
// between releases; some releases number the options and some (Claude Code
// 2.1.299) do not. Instead of assuming "Yes" is highlighted, we read the
// rendered options, find the affirmative one, and move the highlight to it
// before pressing Enter.

// dialogOption is one line of a rendered dialog, e.g. " ❯ No, exit".
type dialogOption struct {
	text     string
	selected bool
}

// dialogLineRe splits a line into "[marker] [N.] text" where marker is the
// TUI's highlight glyph (❯, ›, >, ▶, ➤) and the "N." prefix is optional.
var dialogLineRe = regexp.MustCompile(`^\s*([❯›>▶➤])?\s*(?:(\d+)[.)]\s+)?(\S.*?)\s*$`)

// yesNoLeadRe recognizes an unnumbered, unhighlighted option line.
var yesNoLeadRe = regexp.MustCompile(`^(Yes|No)\b`)

// dialogFooterMarkers end the option list; anything after them is not an option.
var dialogFooterMarkers = []string{"Enter to confirm", "Esc to cancel"}

// parseDialogLine reports whether line is a dialog option. strict limits the
// match to lines that are unmistakably options (numbered, or starting with
// "Yes"/"No"); otherwise a highlight glyph alone also qualifies.
func parseDialogLine(line string, strict bool) (dialogOption, bool) {
	m := dialogLineRe.FindStringSubmatch(line)
	if m == nil {
		return dialogOption{}, false
	}
	marker, number, text := m[1], m[2], m[3]
	if number == "" && !yesNoLeadRe.MatchString(text) && (strict || marker == "") {
		return dialogOption{}, false
	}
	return dialogOption{text: text, selected: marker != ""}, true
}

// parseDialogOptions extracts the options that follow the last non-option line
// containing one of markers, up to the dialog's footer. Anchoring on the last
// marker keeps stale dialogs earlier in the scrollback from being mistaken for
// the live one; stopping at the footer and requiring each line to look like an
// option keeps body prose from being parsed as options.
func parseDialogOptions(content string, markers ...string) []dialogOption {
	lines := strings.Split(content, "\n")
	start := -1
	for i, line := range lines {
		// Option lines can echo a marker ("Yes, I trust this folder"); only
		// the title line anchors the dialog.
		if _, isOption := parseDialogLine(line, true); isOption {
			continue
		}
		for _, m := range markers {
			if strings.Contains(line, m) {
				start = i
			}
		}
	}
	if start < 0 {
		return nil
	}
	var opts []dialogOption
	for _, line := range lines[start+1:] {
		if containsAny(line, dialogFooterMarkers) {
			break
		}
		if o, ok := parseDialogLine(line, false); ok {
			opts = append(opts, o)
		}
	}
	return opts
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// affirmativeOption reports whether an option text accepts the dialog
// ("Yes, I trust this folder", "Yes, continue", "Yes, I accept").
func affirmativeOption(text string) bool {
	return strings.HasPrefix(text, "Yes")
}

// planDialogSelection returns the arrow keys (excluding the final Enter) that
// move the highlight from the pre-selected option to the single affirmative
// option. It errors when the options cannot be identified unambiguously, so
// callers never press Enter on a guess.
func planDialogSelection(opts []dialogOption) ([]string, error) {
	if len(opts) == 0 {
		return nil, fmt.Errorf("no options visible")
	}
	yes, sel := -1, -1
	for i, o := range opts {
		if affirmativeOption(o.text) {
			if yes >= 0 {
				return nil, fmt.Errorf("multiple affirmative options visible")
			}
			yes = i
		}
		if o.selected {
			if sel >= 0 {
				return nil, fmt.Errorf("multiple highlighted options visible")
			}
			sel = i
		}
	}
	if yes < 0 {
		return nil, fmt.Errorf("no affirmative (\"Yes...\") option among %s", describeOptions(opts))
	}
	if sel < 0 {
		return nil, fmt.Errorf("cannot tell which option is highlighted among %s", describeOptions(opts))
	}
	var keys []string
	for ; sel < yes; sel++ {
		keys = append(keys, "Down")
	}
	for ; sel > yes; sel-- {
		keys = append(keys, "Up")
	}
	return keys, nil
}

func describeOptions(opts []dialogOption) string {
	texts := make([]string, len(opts))
	for i, o := range opts {
		texts[i] = fmt.Sprintf("%q", o.text)
	}
	return strings.Join(texts, ", ")
}

// workspaceTrustMarkers are the title lines that introduce a trust dialog.
var workspaceTrustMarkers = []string{
	"trust this folder",
	"Quick safety check",
	"Do you trust the contents of this directory?",
}

var bypassPermissionsMarkers = []string{"Bypass Permissions mode"}

// selectAffirmativeOption highlights the affirmative option of the dialog
// introduced by markers and presses Enter. If the options are not (yet)
// parseable it returns an error without sending any key.
func (t *Tmux) selectAffirmativeOption(session, content string, markers []string) error {
	keys, err := planDialogSelection(parseDialogOptions(content, markers...))
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := t.run("send-keys", "-t", session, k); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, err = t.run("send-keys", "-t", session, "Enter")
	return err
}
