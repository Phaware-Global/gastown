package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWispID(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
	}{
		{
			name:   "standard wisp output",
			input:  "✓ Spawned wisp: gt-wisp-abc123 — Reap stale wisps",
			wantID: "gt-wisp-abc123",
		},
		{
			name:   "wisp ID with ANSI codes",
			input:  "\033[32m✓\033[0m Spawned wisp: \033[1mgt-wisp-xyz789\033[0m — Title",
			wantID: "gt-wisp-xyz789",
		},
		{
			name:   "empty output",
			input:  "",
			wantID: "",
		},
		{
			name:   "no wisp ID in output",
			input:  "Error: something went wrong",
			wantID: "",
		},
		{
			name:   "wisp ID at end of line",
			input:  "Created gt-wisp-def456",
			wantID: "gt-wisp-def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWispID(tt.input)
			if got != tt.wantID {
				t.Errorf("parseWispID(%q) = %q, want %q", tt.input, got, tt.wantID)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no ANSI", "hello", "hello"},
		{"color code", "\033[32mgreen\033[0m", "green"},
		{"bold", "\033[1mbold\033[0m", "bold"},
		{"multiple codes", "\033[32m✓\033[0m \033[1mtext\033[0m", "✓ text"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseChildrenJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCount int
		wantErr   bool
	}{
		{
			name:      "bare array",
			input:     `[{"id":"a","title":"Probe","status":"open"}]`,
			wantCount: 1,
		},
		{
			name:      "map wrapper from bd show",
			input:     `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"},{"id":"hq-wisp-b","title":"Report","status":"open"}]}`,
			wantCount: 2,
		},
		{
			name:      "empty map wrapper",
			input:     `{"hq-wisp-root":[]}`,
			wantCount: 0,
		},
		{
			name:      "empty array",
			input:     `[]`,
			wantCount: 0,
		},
		{
			name:      "bd >=1.0.3 envelope with schema_version sibling",
			input:     `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}],"schema_version":1}`,
			wantCount: 1,
		},
		{
			name:      "bd >=1.0.3 envelope with empty children",
			input:     `{"hq-wisp-root":[],"schema_version":1}`,
			wantCount: 0,
		},
		{
			name:    "envelope with no array-shaped sibling errors",
			input:   `{"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "envelope with null parent value errors (not silent nil slice)",
			input:   `{"hq-wisp-root":null,"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChildrenJSON(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if len(got) != tt.wantCount {
				t.Errorf("got %d children, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestDogMolGracefulDegradation(t *testing.T) {
	// A dogMol with empty rootID should be a no-op for all operations.
	dm := &dogMol{
		rootID:  "",
		stepIDs: make(map[string]string),
	}

	// These should not panic or error — graceful degradation.
	dm.closeStep("scan")
	dm.failStep("scan", "test failure")
	dm.close()
}

// newFakeBdMol returns a dogMol whose bd is a shell script modelling a blocks
// chain: `bd close X` fails with bd's real "blocked by open issues" error until
// X's blocker (a file blocker.X in the state dir) has been closed. Children are
// listed as all-open in the order given. Successful closes are appended to
// close.log in the state dir.
func newFakeBdMol(t *testing.T, childIDs []string, blockers map[string]string) (*dogMol, string) {
	t.Helper()
	dir := t.TempDir()

	var items []string
	for _, id := range childIDs {
		items = append(items, fmt.Sprintf(`{"id":%q,"title":"step","status":"open"}`, id))
	}
	children := fmt.Sprintf(`{"root-wisp":[%s],"schema_version":1}`, strings.Join(items, ","))
	if err := os.WriteFile(filepath.Join(dir, "children.json"), []byte(children), 0o644); err != nil {
		t.Fatal(err)
	}
	for id, blocker := range blockers {
		if err := os.WriteFile(filepath.Join(dir, "blocker."+id), []byte(blocker), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	script := fmt.Sprintf(`#!/bin/sh
DIR=%q
case "$1" in
show) cat "$DIR/children.json" ;;
close)
  id="$2"
  blocker=$(cat "$DIR/blocker.$id" 2>/dev/null)
  if [ -n "$blocker" ] && [ ! -f "$DIR/closed.$blocker" ]; then
    echo "cannot close $id: blocked by open issues [$blocker] (use --force to override)" >&2
    exit 1
  fi
  touch "$DIR/closed.$id"
  echo "$id" >> "$DIR/close.log"
  ;;
esac
`, dir)
	bdPath := filepath.Join(dir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return &dogMol{
		rootID:   "root-wisp",
		stepIDs:  make(map[string]string),
		bdPath:   bdPath,
		townRoot: dir,
		logger:   log.New(io.Discard, "", 0),
	}, dir
}

func readCloseLog(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "close.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// bd lists children in an order that is the reverse of the blocks chain, so a
// single in-order pass would be refused on every step but the first one tried.
func TestDogMolCloseDependencyOrder(t *testing.T) {
	dm, dir := newFakeBdMol(t,
		[]string{"s3", "s2", "s1"},
		map[string]string{"s3": "s2", "s2": "s1"})

	dm.close()

	got := strings.Join(readCloseLog(t, dir), ",")
	if want := "s1,s2,s3,root-wisp"; got != want {
		t.Errorf("close order = %s, want %s", got, want)
	}
}

// A step blocked by something outside the molecule can never close; the root
// must stay open so the leaked step is not orphaned.
func TestDogMolCloseKeepsRootWhenStepsRemain(t *testing.T) {
	dm, dir := newFakeBdMol(t,
		[]string{"s2", "s1"},
		map[string]string{"s2": "s1", "s1": "external"})

	dm.close()

	if got := readCloseLog(t, dir); len(got) != 0 {
		t.Errorf("closed %v, want nothing closed", got)
	}
	if left := dm.closeRemainingSteps(); left != 2 {
		t.Errorf("closeRemainingSteps left = %d, want 2", left)
	}
}

// A partially closable molecule closes what it can but still keeps the root.
func TestDogMolClosePartialProgressKeepsRoot(t *testing.T) {
	dm, dir := newFakeBdMol(t,
		[]string{"s2", "s1", "s0"},
		map[string]string{"s2": "s1", "s1": "external"})

	dm.close()

	got := strings.Join(readCloseLog(t, dir), ",")
	if want := "s0"; got != want {
		t.Errorf("closed = %s, want %s (root must stay open)", got, want)
	}
}

func TestIsCloseBlocked(t *testing.T) {
	if isCloseBlocked(nil) {
		t.Error("nil error reported as blocked")
	}
	if !isCloseBlocked(fmt.Errorf("exit status 1: cannot close a: blocked by open issues [b] (use --force to override)")) {
		t.Error("blocked error not recognized")
	}
	if isCloseBlocked(fmt.Errorf("exit status 1: dolt connection refused")) {
		t.Error("transient error reported as blocked")
	}
}
