package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// installLabelBD puts a fake bd on PATH. Writes apply to the returned store
// file, one label per line. "show" reports shown instead of the store, which is
// what a reader sees while another writer is part-way through replacing the set.
func installLabelBD(t *testing.T, stored, shown []string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	store := filepath.Join(dir, "labels")
	if err := os.WriteFile(store, []byte(strings.Join(stored, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	showJSON := `[{"labels":["` + strings.Join(shown, `","`) + `"]}]`
	script := `#!/bin/sh
store='` + store + `'
case "$1" in
show) echo '` + showJSON + `' ;;
update)
  shift 2
  replaced=
  for arg in "$@"; do
    value=${arg#*=}
    case "$arg" in
    --set-labels=*)
      [ -n "$replaced" ] || : > "$store"
      replaced=1
      echo "$value" >> "$store" ;;
    --add-label=*)
      grep -qxF -- "$value" "$store" || echo "$value" >> "$store" ;;
    --remove-label=*)
      grep -vxF -- "$value" "$store" > "$store.tmp"
      mv "$store.tmp" "$store" ;;
    *) exit 1 ;;
    esac
  done ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return store
}

func storedLabels(t *testing.T, store string) []string {
	t.Helper()
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	labels := strings.Fields(string(data))
	slices.Sort(labels)
	return labels
}

func TestAgentLabelWritersPreserveOtherLabels(t *testing.T) {
	stored := []string{"backoff-until:100", "gt:agent", "heartbeat:100", "idle:1"}
	// gt:agent is absent from what the writer reads.
	partial := []string{"backoff-until:100", "heartbeat:100", "idle:1"}

	tests := []struct {
		name  string
		write func(beadsDir string) error
		want  string // a label the write must leave behind
		gone  string // a label the write must remove
	}{
		{
			name:  "idle increment",
			write: func(dir string) error { return setAgentIdleCycles("x-refinery", dir, 2) },
			want:  "idle:2",
			gone:  "idle:1",
		},
		{
			name:  "backoff set",
			write: func(dir string) error { return setAgentBackoffUntil("x-refinery", dir, time.Unix(200, 0)) },
			want:  "backoff-until:200",
			gone:  "backoff-until:100",
		},
		{
			name:  "backoff clear",
			write: func(dir string) error { return clearAgentBackoffUntil("x-refinery", dir) },
			want:  "idle:1",
			gone:  "backoff-until:100",
		},
		{
			name:  "heartbeat",
			write: func(dir string) error { return updateAgentHeartbeat("x-refinery", dir) },
			want:  "idle:1",
			gone:  "heartbeat:100",
		},
		{
			name: "agent state increment",
			write: func(dir string) error {
				agentStateIncr = "idle"
				defer func() { agentStateIncr = "" }()
				return modifyAgentState("x-refinery", dir, true)
			},
			want: "idle:2",
			gone: "idle:1",
		},
		{
			name: "agent state delete",
			write: func(dir string) error {
				agentStateDel = []string{"idle"}
				defer func() { agentStateDel = nil }()
				return modifyAgentState("x-refinery", dir, false)
			},
			want: "heartbeat:100",
			gone: "idle:1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := installLabelBD(t, stored, partial)
			if err := tt.write(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			got := storedLabels(t, store)
			if !slices.Contains(got, "gt:agent") {
				t.Errorf("gt:agent lost: %v", got)
			}
			if !slices.Contains(got, tt.want) {
				t.Errorf("missing %s: %v", tt.want, got)
			}
			if slices.Contains(got, tt.gone) {
				t.Errorf("%s not removed: %v", tt.gone, got)
			}
		})
	}
}

// Several labels share the "gt" prefix; a state write must not collapse them.
func TestAgentStateKeepsLabelsSharingAPrefix(t *testing.T) {
	labels := []string{"gt:agent", "gt:other", "idle:1"}
	store := installLabelBD(t, labels, labels)

	agentStateSet = []string{"idle=0"}
	defer func() { agentStateSet = nil }()
	if err := modifyAgentState("x-refinery", t.TempDir(), false); err != nil {
		t.Fatal(err)
	}

	want := []string{"gt:agent", "gt:other", "idle:0"}
	if got := storedLabels(t, store); !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

// A change to the "gt" key removes its other labels but never gt:agent.
func TestAgentStateGtKeyChangesKeepGtAgent(t *testing.T) {
	labels := []string{"gt:agent", "gt:other", "idle:1"}

	tests := []struct {
		name     string
		set, del []string
		want     []string
	}{
		{name: "delete", del: []string{"gt"}, want: []string{"gt:agent", "idle:1"}},
		{name: "set", set: []string{"gt=x"}, want: []string{"gt:agent", "gt:x", "idle:1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := installLabelBD(t, labels, labels)
			agentStateSet, agentStateDel = tt.set, tt.del
			defer func() { agentStateSet, agentStateDel = nil, nil }()
			if err := modifyAgentState("x-refinery", t.TempDir(), false); err != nil {
				t.Fatal(err)
			}
			if got := storedLabels(t, store); !slices.Equal(got, tt.want) {
				t.Errorf("labels = %v, want %v", got, tt.want)
			}
		})
	}
}

// A process bd leaves behind holding its output pipes must not stall the call,
// and the call still succeeds because bd itself did.
func TestAgentStateWriteDoesNotWaitForBdChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
sleep 20 &
[ "$1" != show ] || echo '[{"labels":["gt:agent"]}]'
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := bdWaitDelay
	bdWaitDelay = 100 * time.Millisecond
	defer func() { bdWaitDelay = old }()

	start := time.Now()
	if err := setAgentIdleCycles("x-refinery", t.TempDir(), 1); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("write took %v", elapsed)
	}
}
