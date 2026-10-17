package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBDScript emulates the slice of `bd mol wisp` the check uses against a
// directory of wisps (one file per wisp; the file's content is its status and
// its name is the wisp ID). `gc` without --closed deletes every non-closed
// wisp, as real bd does for abandoned wisps; `gc --closed --force` deletes
// closed wisps only.
const fakeBDScript = `#!/bin/sh
state="$FAKE_BD_STATE"
[ "$1 $2" = "mol wisp" ] || exit 0
shift 2
sub="$1"; shift
all=0; closed=0; force=0
for a in "$@"; do
  case "$a" in
    --all) all=1 ;;
    --closed) closed=1 ;;
    --force|-f) force=1 ;;
  esac
done
case "$sub" in
  list)
    printf '{"schema_version":1,"wisps":['
    sep=""
    for f in "$state"/*; do
      [ -e "$f" ] || continue
      st=$(cat "$f")
      [ "$st" = closed ] && [ $all = 0 ] && continue
      printf '%s{"id":"%s","status":"%s","ephemeral":true,"updated_at":"2020-01-01T00:00:00Z"}' "$sep" "$(basename "$f")" "$st"
      sep=","
    done
    printf ']}\n'
    ;;
  gc)
    if [ $closed = 1 ]; then
      [ $force = 1 ] || exit 0
      for f in "$state"/*; do [ "$(cat "$f")" = closed ] && rm -f "$f"; done
    else
      for f in "$state"/*; do [ "$(cat "$f")" != closed ] && rm -f "$f"; done
    fi
    ;;
esac
exit 0
`

func setupFakeBDTown(t *testing.T, wisps map[string]string) (townRoot, stateDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd")
	}
	townRoot = t.TempDir()
	stateDir = t.TempDir()

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBDScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_BD_STATE", stateDir)

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := `{"version":1,"rigs":{"myrig":{"git_url":"x","added_at":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig"), 0o755); err != nil {
		t.Fatal(err)
	}
	for id, status := range wisps {
		if err := os.WriteFile(filepath.Join(stateDir, id), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return townRoot, stateDir
}

func survivingWisps(t *testing.T, stateDir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// An open wisp (merge-request, molecule-attached, hooked, ...) last updated
// long ago must survive `gt doctor --fix`: the 2026-10-14 incident wiped every
// open merge-request wisp town-wide through this path.
func TestWispGCCheck_FixNeverDeletesOpenWisps(t *testing.T) {
	townRoot, stateDir := setupFakeBDTown(t, map[string]string{
		"mr-open":       "open",
		"mol-hooked":    "hooked",
		"patrol-active": "in_progress",
		"done-1":        "closed",
	})
	ctx := &CheckContext{TownRoot: townRoot}
	c := NewWispGCCheck()

	res := c.Run(ctx)
	if res.Status == StatusError {
		t.Fatalf("Run failed: %s %v", res.Message, res.Details)
	}
	// Mirror `gt doctor --fix`: Fix runs whenever the check is not OK.
	if res.Status != StatusOK {
		if err := c.Fix(ctx); err != nil {
			t.Fatalf("Fix: %v", err)
		}
	}

	left := survivingWisps(t, stateDir)
	for _, id := range []string{"mr-open", "mol-hooked", "patrol-active"} {
		if !left[id] {
			t.Errorf("non-closed wisp %q was deleted by doctor --fix", id)
		}
	}
	if left["done-1"] {
		t.Errorf("closed wisp should still be reclaimed by doctor --fix")
	}
}

// With only stale open wisps there is nothing safe to fix: the check must not
// ask for --fix, and Fix must leave them alone even when called directly.
func TestWispGCCheck_StaleOpenOnlyIsReportOnly(t *testing.T) {
	townRoot, stateDir := setupFakeBDTown(t, map[string]string{"mr-open": "open"})
	ctx := &CheckContext{TownRoot: townRoot}
	c := NewWispGCCheck()

	res := c.Run(ctx)
	if res.Status != StatusOK {
		t.Errorf("status = %v, want OK (report-only); msg=%q", res.Status, res.Message)
	}
	if !strings.Contains(strings.Join(res.Details, "\n"), "stale open") {
		t.Errorf("stale open wisps should be reported in details, got %v", res.Details)
	}
	if res.FixHint != "" {
		t.Errorf("no --fix hint expected for open wisps, got %q", res.FixHint)
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !survivingWisps(t, stateDir)["mr-open"] {
		t.Error("Fix deleted an open wisp")
	}
}
