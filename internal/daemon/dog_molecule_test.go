package daemon

import (
	"os"
	"path/filepath"
	"runtime"
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

// fakeBlockingBD writes a stand-in `bd` that models the real CLI's dependency
// guard: `bd close X` fails with "blocked by open issues" while any of X's
// blockers is still open, and succeeds otherwise. `show <root> --children`
// lists the children in the order given by children (each "id:title"), with
// blockers wired as blockedBy[id] = blocker. Any `--force` makes the fake
// record a violation, because completion must never override the guard.
func fakeBlockingBD(t *testing.T, root string, children []string, blockedBy map[string]string) (bdPath, stateDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	stateDir = t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root+".status", "open")
	var listing []string
	for _, c := range children {
		id, title, _ := strings.Cut(c, ":")
		write(id+".status", "open")
		write(id+".title", title)
		listing = append(listing, id)
	}
	for id, blocker := range blockedBy {
		write(id+".blocker", blocker)
	}
	write("order", strings.Join(listing, "\n")+"\n")

	script := `#!/bin/sh
S="` + stateDir + `"
case "$1" in
show)
  printf '{"%s": [' "$2"
  first=1
  while read -r id; do
    [ -n "$id" ] || continue
    [ $first -eq 1 ] || printf ','
    first=0
    printf '{"id":"%s","title":"%s","status":"%s"}' "$id" "$(cat "$S/$id.title")" "$(cat "$S/$id.status")"
  done < "$S/order"
  printf '], "schema_version": 1}\n'
  ;;
close)
  id="$2"
  for a in "$@"; do [ "$a" = "--force" ] && echo "$id" >> "$S/forced"; done
  if [ -f "$S/$id.blocker" ] && [ "$(cat "$S/$(cat "$S/$id.blocker").status")" != "closed" ]; then
    echo "cannot close $id: blocked by open issues [$(cat "$S/$id.blocker")] (use --force to override)" >&2
    exit 1
  fi
  printf closed > "$S/$id.status"
  ;;
*)
  echo "unexpected bd call: $*" >&2
  exit 2
  ;;
esac
`
	bdPath = filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bdPath, stateDir
}

func readState(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...interface{}) { l.t.Logf(format, args...) }

// TestDogMolCloseLeavesNoOpenStepsWhenListedOutOfDependencyOrder is the RED
// test for gt-78kgc: steps form a blocks chain, and bd lists a later step
// before the step that blocks it. A single listing-order pass fails to close
// the later step (blocked) and never revisits it once its blocker closes,
// leaving it open under a closed root.
func TestDogMolCloseLeavesNoOpenStepsWhenListedOutOfDependencyOrder(t *testing.T) {
	bdPath, state := fakeBlockingBD(t, "root",
		[]string{"report:Report findings and return to kennel", "push:Commit and push to git archive", "export:Export databases to JSONL"},
		map[string]string{"report": "push", "push": "export"},
	)
	dm := &dogMol{
		rootID:   "root",
		stepIDs:  map[string]string{},
		bdPath:   bdPath,
		townRoot: t.TempDir(),
		logger:   testLogger{t},
	}

	dm.close()

	for _, id := range []string{"report", "push", "export", "root"} {
		if got := readState(t, state, id+".status"); got != "closed" {
			t.Errorf("%s status = %q, want closed", id, got)
		}
	}
	if forced := readState(t, state, "forced"); forced != "" {
		t.Errorf("completion used --force on: %s", forced)
	}
}

// A step whose blocker can never close (the blocker's own close keeps failing)
// must stay open without looping forever, and must not be force-closed.
func TestDogMolCloseTerminatesOnPermanentlyBlockedStep(t *testing.T) {
	bdPath, state := fakeBlockingBD(t, "root",
		[]string{"b:Report findings", "a:Commit and push"},
		map[string]string{"b": "a", "a": "b"},
	)
	dm := &dogMol{rootID: "root", stepIDs: map[string]string{}, bdPath: bdPath, townRoot: t.TempDir(), logger: testLogger{t}}

	dm.close()

	for _, id := range []string{"a", "b"} {
		if got := readState(t, state, id+".status"); got != "open" {
			t.Errorf("%s status = %q, want open (cyclic blockers cannot close without --force)", id, got)
		}
	}
	if forced := readState(t, state, "forced"); forced != "" {
		t.Errorf("completion used --force on: %s", forced)
	}
}
