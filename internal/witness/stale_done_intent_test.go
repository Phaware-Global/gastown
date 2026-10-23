package witness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reuseFixture is a rig with one live polecat session whose agent bead carries
// done-intent labels. Everything that could reach the real town is replaced:
// tmux is a PATH stub, bd is an in-memory fake, and session restarts are
// recorded instead of run.
type reuseFixture struct {
	t        *testing.T
	town     string
	restarts []string
	updates  []string // every `bd update` argument list, joined
	labels   []string
	hookAt   time.Time
}

const reuseHookBead = "al-77"

func newReuseFixture(t *testing.T, hookAssignedAgo time.Duration, labelAges ...time.Duration) *reuseFixture {
	t.Helper()
	f := &reuseFixture{t: t, town: t.TempDir(), hookAt: time.Now().Add(-hookAssignedAgo)}
	for _, age := range labelAges {
		f.labels = append(f.labels, fmt.Sprintf("done-intent:COMPLETED:%d", time.Now().Add(-age).Unix()))
	}
	if err := os.MkdirAll(filepath.Join(f.town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.town, "alpha", "polecats", "nux"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A tmux that says every session is alive and running the agent, so the only
	// thing that can make the detector act is the done-intent decision.
	stubs := t.TempDir()
	tmuxStub := `#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    has-session) exit 0 ;;
    show-environment) case "$*" in *GT_PROCESS_NAMES*) echo "GT_PROCESS_NAMES=claude"; exit 0 ;; *) exit 1 ;; esac ;;
    display-message|list-panes) echo "claude"; exit 0 ;;
    list-sessions) exit 0 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(stubs, "tmux"), []byte(tmuxStub), 0o755); err != nil { //nolint:gosec // G306: test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	orig := restartPolecatSession
	restartPolecatSession = func(_, rig, name string) error {
		f.restarts = append(f.restarts, rig+"/"+name)
		return nil
	}
	t.Cleanup(func() { restartPolecatSession = orig })
	return f
}

func (f *reuseFixture) bd() *BdCli {
	show := func(id string) string {
		var issue map[string]any
		if strings.Contains(id, "polecat-nux") {
			issue = map[string]any{
				"agent_state": "working", "hook_bead": reuseHookBead,
				"labels": f.labels, "updated_at": time.Now().Format(time.RFC3339),
				"description": "role_type: polecat\nrig: alpha\nagent_state: working\nhook_bead: " + reuseHookBead,
			}
		} else {
			issue = map[string]any{
				"id": id, "status": "hooked",
				"description": "attached_formula: mol-polecat-work\nattached_at: " + f.hookAt.UTC().Format(time.RFC3339),
			}
		}
		b, err := json.Marshal([]any{issue})
		if err != nil {
			f.t.Fatal(err)
		}
		return string(b)
	}
	record := func(args []string) {
		if len(args) > 0 && args[0] == "update" {
			f.updates = append(f.updates, strings.Join(args, " "))
		}
	}
	return &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			record(args)
			if len(args) >= 2 && args[0] == "show" {
				return show(args[1]), nil
			}
			return "[]", nil
		},
		Run: func(_ string, args ...string) error {
			record(args)
			return nil
		},
	}
}

func (f *reuseFixture) detect() *DetectZombiePolecatsResult {
	f.t.Helper()
	return DetectZombiePolecats(f.bd(), f.town, "alpha", nil)
}

// removedLabels returns every label a `bd update` asked to remove.
func (f *reuseFixture) removedLabels() map[string]bool {
	removed := map[string]bool{}
	for _, u := range f.updates {
		for _, arg := range strings.Fields(u) {
			if label, ok := strings.CutPrefix(arg, "--remove-label="); ok {
				removed[label] = true
			}
		}
	}
	return removed
}

// A polecat name reused for a new assignment inherits the previous task's
// done-intent label. Work assigned after the label was written is not stuck in
// done, and restarting it kills live work.
func TestDetectZombiePolecats_StaleDoneIntentOnReusedPolecatIsNotRestarted(t *testing.T) {
	f := newReuseFixture(t, time.Hour, 6*24*time.Hour)

	res := f.detect()
	if len(f.restarts) != 0 {
		t.Errorf("restarted a live polecat on a stale done-intent: %v", f.restarts)
	}
	for _, z := range res.Zombies {
		t.Errorf("reported zombie %+v for a polecat working on newer assignment", z)
	}
}

// Labels accumulate when COMPLETED exits are followed by reuse; every one of
// them belongs to a finished task and goes, not just the first.
func TestDetectZombiePolecats_StaleDoneIntentLabelsAreAllCleared(t *testing.T) {
	f := newReuseFixture(t, time.Hour, 6*24*time.Hour, 4*24*time.Hour, 3*time.Hour)

	f.detect()
	removed := f.removedLabels()
	for _, label := range f.labels {
		if !removed[label] {
			t.Errorf("stale label %q was not cleared; removed = %v", label, removed)
		}
	}
}

// A done-intent written after the current assignment is a real gt done that
// stalled, and is still detected and restarted.
func TestDetectZombiePolecats_FreshDoneIntentIsStillDetected(t *testing.T) {
	f := newReuseFixture(t, 2*time.Hour, 5*time.Minute)

	res := f.detect()
	if len(f.restarts) != 1 || f.restarts[0] != "alpha/nux" {
		t.Errorf("restarts = %v, want [alpha/nux]", f.restarts)
	}
	if len(res.Zombies) != 1 || res.Zombies[0].Classification != ZombieStuckInDone {
		t.Errorf("zombies = %+v, want one stuck-in-done", res.Zombies)
	}
	if removed := f.removedLabels(); len(removed) != 0 {
		t.Errorf("cleared a live done-intent: %v", removed)
	}
}

// With a leftover from an earlier task and a fresh label from a gt done that
// has only just started, the newest label decides: the session is inside the
// stuck timeout, so nothing is restarted, and only the leftover is cleared.
func TestDetectZombiePolecats_NewestDoneIntentDecides(t *testing.T) {
	f := newReuseFixture(t, time.Hour, 6*24*time.Hour, 10*time.Second)

	f.detect()
	if len(f.restarts) != 0 {
		t.Errorf("restarted on the old label although the newest is 10s old: %v", f.restarts)
	}
	removed := f.removedLabels()
	if removed[f.labels[1]] {
		t.Errorf("cleared the fresh label %q", f.labels[1])
	}
	if !removed[f.labels[0]] {
		t.Errorf("did not clear the leftover label %q", f.labels[0])
	}
}
