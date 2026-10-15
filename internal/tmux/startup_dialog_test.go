package tmux

import (
	"reflect"
	"strings"
	"testing"
)

// claudeTrustCapture is the trust dialog as Claude Code 2.1.299 renders it:
// options are unnumbered, and "No, exit" is listed first and pre-selected.
const claudeTrustCapture = `
 Accessing workspace:

 /Users/agent/gt/gastown

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

// numberedTrustCapture is the numbered rendering used by other releases and
// by Codex.
const numberedTrustCapture = `
 Quick safety check: Is this a project you created or one you trust?

 Claude Code'll be able to read, edit, and execute files here.

 ❯ 1. No, exit
   2. Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func mustParseDialogOptions(t *testing.T, content string) []dialogOption {
	t.Helper()
	opts, err := parseDialogOptions(content, workspaceTrustMarkers...)
	if err != nil {
		t.Fatalf("parseDialogOptions: %v", err)
	}
	return opts
}

func TestParseDialogOptions(t *testing.T) {
	t.Parallel()
	got := mustParseDialogOptions(t, claudeTrustCapture)
	want := []dialogOption{
		{text: "No, exit", selected: true},
		{text: "Yes, I trust this folder"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseDialogOptions_Numbered(t *testing.T) {
	t.Parallel()
	got := mustParseDialogOptions(t, numberedTrustCapture)
	want := []dialogOption{
		{text: "No, exit", selected: true},
		{text: "Yes, I trust this folder"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Body prose between the title and the options, and the footer after them,
// are not options.
func TestParseDialogOptions_UnnumberedIgnoresProseAndFooter(t *testing.T) {
	t.Parallel()
	content := claudeTrustCapture + "\nNo further changes will be made.\n❯ stray\n"
	got := mustParseDialogOptions(t, content)
	if len(got) != 2 {
		t.Fatalf("got %+v, want exactly the two dialog options", got)
	}
}

// An option line that echoes a marker ("Yes, I trust this folder") must not
// become the anchor, or the options listed above it would be dropped.
func TestParseDialogOptions_OptionLineEchoingMarker(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"numbered":   "Quick safety check\n  1. No, exit\n❯ 2. Yes, I trust this folder\n",
		"unnumbered": "Quick safety check\n  No, exit\n❯ Yes, I trust this folder\n",
	} {
		got := mustParseDialogOptions(t, content)
		if len(got) != 2 {
			t.Errorf("%s: got %+v, want both options", name, got)
		}
	}
}

// Only the most recent dialog counts; a dismissed one in scrollback must not
// contribute options.
func TestParseDialogOptions_IgnoresStaleDialog(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"numbered":   "Quick safety check\n❯ 1. Yes, I trust this folder\n  2. No, exit\nuser@host:~$\nQuick safety check\n❯ 1. No, exit\n  2. Yes, I trust this folder\n",
		"unnumbered": "Quick safety check\n❯ Yes, I trust this folder\n  No, exit\nuser@host:~$\nQuick safety check\n❯ No, exit\n  Yes, I trust this folder\n",
	} {
		got := mustParseDialogOptions(t, content)
		if len(got) != 2 || got[0].text != "No, exit" || !got[0].selected {
			t.Errorf("%s: got %+v, want the latest dialog", name, got)
		}
	}
}

func TestParseDialogOptions_NoMarker(t *testing.T) {
	t.Parallel()
	if got := mustParseDialogOptions(t, "1. foo\n2. bar"); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestPlanDialogSelection(t *testing.T) {
	t.Parallel()
	yes := dialogOption{text: "Yes, I trust this folder"}
	no := dialogOption{text: "No, exit"}
	sel := func(o dialogOption) dialogOption { o.selected = true; return o }

	tests := []struct {
		name    string
		opts    []dialogOption
		want    []string
		wantErr bool
	}{
		{"yes already highlighted (first)", []dialogOption{sel(yes), no}, nil, false},
		{"yes already highlighted (second)", []dialogOption{no, sel(yes)}, nil, false},
		{"no highlighted, yes below", []dialogOption{sel(no), yes}, []string{"Down"}, false},
		{"no highlighted, yes above", []dialogOption{yes, sel(no)}, []string{"Up"}, false},
		{"three options", []dialogOption{sel(no), {text: "Maybe"}, yes}, []string{"Down", "Down"}, false},
		{"no options", nil, nil, true},
		{"no yes option", []dialogOption{sel(no), {text: "Abort"}}, nil, true},
		{"two yes options", []dialogOption{sel(yes), yes}, nil, true},
		{"nothing highlighted", []dialogOption{no, yes}, nil, true},
		{"two highlighted", []dialogOption{sel(no), sel(yes)}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planDialogSelection(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("keys = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanDialogSelection_TrustCaptures(t *testing.T) {
	t.Parallel()
	for name, capture := range map[string]string{
		"unnumbered (2.1.299)": claudeTrustCapture,
		"numbered":             numberedTrustCapture,
	} {
		keys, err := planDialogSelection(mustParseDialogOptions(t, capture))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := []string{"Down"}; !reflect.DeepEqual(keys, want) {
			t.Errorf("%s: keys = %v, want %v", name, keys, want)
		}
	}
}

// A non-blank line between two options that parses as neither is an error that
// names it; guessing past it would skew the arrow count.
func TestParseDialogOptions_UnclassifiableLineBetweenOptions(t *testing.T) {
	t.Parallel()
	content := "Quick safety check\n\n ❯ No, exit\n   Maybe later\n   Yes, I trust this folder\n\n Enter to confirm · Esc to cancel\n"
	opts, err := parseDialogOptions(content, workspaceTrustMarkers...)
	if err == nil {
		t.Fatalf("got %+v, want an error", opts)
	}
	if !strings.Contains(err.Error(), "Maybe later") {
		t.Errorf("error should name the line, got: %v", err)
	}
}

// Lines after the last option (a wrapped footer, trailing prose) are not
// between options and must not fail the parse.
func TestParseDialogOptions_TrailingProseAfterLastOptionIsIgnored(t *testing.T) {
	t.Parallel()
	content := "Quick safety check\n ❯ No, exit\n   Yes, I trust this folder\n\n Some trailing note\n"
	opts, err := parseDialogOptions(content, workspaceTrustMarkers...)
	if err != nil || len(opts) != 2 {
		t.Fatalf("got %+v, %v; want both options and no error", opts, err)
	}
}

func TestConfirmAffirmativeHighlighted(t *testing.T) {
	t.Parallel()
	yes := dialogOption{text: "Yes, I trust this folder"}
	no := dialogOption{text: "No, exit"}
	sel := func(o dialogOption) dialogOption { o.selected = true; return o }
	tests := []struct {
		name    string
		opts    []dialogOption
		wantErr bool
	}{
		{"yes highlighted", []dialogOption{no, sel(yes)}, false},
		{"no highlighted", []dialogOption{sel(no), yes}, true},
		{"nothing highlighted", []dialogOption{no, yes}, true},
		{"no options", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := confirmAffirmativeHighlighted(tt.opts); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
