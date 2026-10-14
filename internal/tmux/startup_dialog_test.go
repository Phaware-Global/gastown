package tmux

import (
	"reflect"
	"testing"
)

// claudeTrustCapture mirrors Claude Code 2.1.299: "No, exit" is listed first
// and pre-selected.
const claudeTrustCapture = `
 Accessing workspace:

 /Users/agent/gt/gastown

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 ❯ 1. No, exit
   2. Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func TestParseDialogOptions(t *testing.T) {
	t.Parallel()
	got := parseDialogOptions(claudeTrustCapture, workspaceTrustMarkers...)
	want := []dialogOption{
		{text: "No, exit", selected: true},
		{text: "Yes, I trust this folder"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An option line that echoes a marker ("Yes, I trust this folder") must not
// become the anchor, or the options listed above it would be dropped.
func TestParseDialogOptions_OptionLineEchoingMarker(t *testing.T) {
	t.Parallel()
	content := "Quick safety check\n  1. No, exit\n❯ 2. Yes, I trust this folder\n"
	got := parseDialogOptions(content, workspaceTrustMarkers...)
	if len(got) != 2 {
		t.Fatalf("got %+v, want both options", got)
	}
}

// Only the most recent dialog counts; a dismissed one in scrollback must not
// contribute options.
func TestParseDialogOptions_IgnoresStaleDialog(t *testing.T) {
	t.Parallel()
	content := "Quick safety check\n❯ 1. Yes, I trust this folder\n  2. No, exit\nuser@host:~$\nQuick safety check\n❯ 1. No, exit\n  2. Yes, I trust this folder\n"
	got := parseDialogOptions(content, workspaceTrustMarkers...)
	if len(got) != 2 || got[0].text != "No, exit" || !got[0].selected {
		t.Fatalf("got %+v, want the latest dialog", got)
	}
}

func TestParseDialogOptions_NoMarker(t *testing.T) {
	t.Parallel()
	if got := parseDialogOptions("1. foo\n2. bar", workspaceTrustMarkers...); got != nil {
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

func TestPlanDialogSelection_ClaudeTrustCapture(t *testing.T) {
	t.Parallel()
	keys, err := planDialogSelection(parseDialogOptions(claudeTrustCapture, workspaceTrustMarkers...))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Down"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}
