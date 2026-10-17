package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stuckAgentDogStubs are PATH stubs for the commands run.sh shells out to.
// Every call is appended to $CALLS so the test can count process spawns.
// `gt hook show` sleeps like the real command does after a town-wide down
// (~5s), so a regression to per-polecat lookups blows the test's deadline as
// well as its call-count assertions.
var stuckAgentDogStubs = map[string]string{
	"tmux": `#!/usr/bin/env bash
echo "tmux $*" >> "$CALLS"
case "$1" in
  has-session) exit 1 ;;
esac
exit 0
`,
	"bd": `#!/usr/bin/env bash
echo "bd $*" >> "$CALLS"
case "$1" in
  list)
    case "$*" in
      *--status=hooked*) cat "$HOOKED_JSON" ;;
      *) echo '[]' ;;
    esac
    ;;
  show) echo '[{"status":"hooked"}]' ;;
  create) echo "wisp-1" ;;
esac
exit 0
`,
	"gt": `#!/usr/bin/env bash
echo "gt $*" >> "$CALLS"
case "$1 $2" in
  "hook show") sleep 5 ;;
  "mail send") cat > /dev/null ;;
esac
exit 0
`,
}

func TestStuckAgentDogBatchesHookLookups(t *testing.T) {
	for _, bin := range []string{"bash", "jq", "awk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}

	const (
		deadPolecats    = 134
		hookedPolecats  = 5
		timeoutSeconds  = 120
		maxBdListCalls  = 4 // {hooked, in_progress} x {one rig, town}
		rigName         = "alpha"
		rigBeadsPrefix  = "al"
		runScriptRelDir = "../../plugins/stuck-agent-dog/run.sh"
	)

	town := t.TempDir()
	stubs := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls.log")

	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := fmt.Sprintf(`{"rigs":{%q:{"beads":{"prefix":%q}}}}`, rigName, rigBeadsPrefix)
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}

	// Many polecat dirs with dead sessions; only a few have hooked work.
	var hooked []string
	for i := 0; i < deadPolecats; i++ {
		name := fmt.Sprintf("p%03d", i)
		if err := os.MkdirAll(filepath.Join(town, rigName, "polecats", name), 0o755); err != nil {
			t.Fatal(err)
		}
		if i < hookedPolecats {
			hooked = append(hooked, fmt.Sprintf(
				`{"id":"al-%d","status":"hooked","assignee":"%s/polecats/%s"}`, i, rigName, name))
		}
	}
	hookedJSON := filepath.Join(t.TempDir(), "hooked.json")
	if err := os.WriteFile(hookedJSON, []byte("["+strings.Join(hooked, ",")+"]"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, body := range stuckAgentDogStubs {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil { //nolint:gosec // G306: test stub must be executable
			t.Fatal(err)
		}
	}

	script, err := filepath.Abs(runScriptRelDir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeoutSeconds*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GT_TOWN_ROOT="+town,
		"CALLS="+calls,
		"HOOKED_JSON="+hookedJSON,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run.sh failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)

	count := func(prefix string) int {
		n := 0
		for _, line := range strings.Split(log, "\n") {
			if strings.HasPrefix(line, prefix) {
				n++
			}
		}
		return n
	}

	if n := count("gt hook show"); n != 0 {
		t.Errorf("run.sh spawned `gt hook show` %d times; hook lookups must be batched", n)
	}
	// `bd show` re-checks only polecats that already look crashed, never the
	// ~130 dead sessions with no hooked work.
	if n := count("bd show"); n > hookedPolecats {
		t.Errorf("run.sh spawned `bd show` %d times, want at most %d (one per hooked polecat)", n, hookedPolecats)
	}
	if n := count("bd list"); n < 1 || n > maxBdListCalls {
		t.Errorf("bd list called %d times, want 1..%d (per rig, not per polecat)", n, maxBdListCalls)
	}
	// Each polecat with hooked work and a dead session is reported to its witness.
	if n := count("gt mail send " + rigName + "/witness"); n != hookedPolecats {
		t.Errorf("restart mails = %d, want %d\n%s", n, hookedPolecats, out)
	}
	if want := fmt.Sprintf("Polecat health: %d crashed", hookedPolecats); !strings.Contains(string(out), want) {
		t.Errorf("output missing %q:\n%s", want, out)
	}
	// The plugin's own timeout is 5m; the per-polecat form needed 11+ minutes,
	// so finishing inside the context deadline above is the timing guard.
}

// dogFixture is a throwaway town with a PATH-stubbed bd/gt/tmux. The bd stub
// answers from files under $DATA keyed by the beads location it was run in
// (a rig name, or "town"):
//
//	list.<loc>.<status>.json  `bd list --status=<status>`
//	sql.<loc>.json            `bd sql` (the wisps table)
//	show.<bead>.json          `bd show <bead>`
type dogFixture struct {
	t     *testing.T
	town  string
	data  string
	calls string
}

const dogBdStub = `#!/usr/bin/env bash
echo "bd $*" >> "$CALLS"
case "$PWD" in
  */mayor/rig) loc=$(basename "$(dirname "$(dirname "$PWD")")") ;;
  *) loc=town ;;
esac
emit() { if [ -f "$1" ]; then cat "$1"; else echo '[]'; fi; }
case "$1" in
  list)
    status=""
    for a in "$@"; do case "$a" in --status=*) status="${a#--status=}" ;; esac; done
    emit "$DATA/list.$loc.$status.json" ;;
  sql) emit "$DATA/sql.$loc.json" ;;
  show) emit "$DATA/show.$2.json" ;;
  create) echo "wisp-1" ;;
esac
exit 0
`

func newDogFixture(t *testing.T, rigs map[string]string) *dogFixture {
	t.Helper()
	for _, bin := range []string{"bash", "jq", "awk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	f := &dogFixture{t: t, town: t.TempDir(), data: t.TempDir(), calls: filepath.Join(t.TempDir(), "calls.log")}

	rigJSON := make([]string, 0, len(rigs))
	for rig, prefix := range rigs {
		rigJSON = append(rigJSON, fmt.Sprintf(`%q:{"beads":{"prefix":%q}}`, rig, prefix))
		f.mkdir(filepath.Join(rig, "mayor", "rig"))
	}
	f.mkdir("mayor")
	f.write(filepath.Join(f.town, "mayor", "rigs.json"), `{"rigs":{`+strings.Join(rigJSON, ",")+`}}`)
	return f
}

func (f *dogFixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.town, rel), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dogFixture) write(path, body string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // G306: test fixture
		f.t.Fatal(err)
	}
}

func (f *dogFixture) polecat(rig, name string) { f.mkdir(filepath.Join(rig, "polecats", name)) }

// hook records one bead row for `bd list` at loc.
func (f *dogFixture) listRows(loc, status string, rows ...map[string]string) {
	f.t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.data, fmt.Sprintf("list.%s.%s.json", loc, status)), string(b))
}

func (f *dogFixture) wispRows(loc string, rows ...map[string]string) {
	f.t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.data, fmt.Sprintf("sql.%s.json", loc)), string(b))
}

func (f *dogFixture) beadStatus(bead, status string) {
	f.write(filepath.Join(f.data, "show."+bead+".json"), fmt.Sprintf(`[{"id":%q,"status":%q}]`, bead, status))
}

// run executes run.sh and returns its output and the stub call log.
func (f *dogFixture) run() (out, calls string) {
	f.t.Helper()
	stubs := f.t.TempDir()
	for name, body := range map[string]string{
		"tmux": "#!/usr/bin/env bash\necho \"tmux $*\" >> \"$CALLS\"\ncase \"$1\" in has-session) exit 1 ;; esac\nexit 0\n",
		"bd":   dogBdStub,
		"gt":   "#!/usr/bin/env bash\necho \"gt $*\" >> \"$CALLS\"\ncase \"$1 $2\" in \"mail send\") cat > /dev/null ;; esac\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil { //nolint:gosec // G306: test stub must be executable
			f.t.Fatal(err)
		}
	}
	script, err := filepath.Abs("../../plugins/stuck-agent-dog/run.sh")
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GT_TOWN_ROOT="+f.town, "CALLS="+f.calls, "DATA="+f.data)
	b, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("run.sh failed: %v\n%s", err, b)
	}
	raw, _ := os.ReadFile(f.calls)
	return string(b), string(raw)
}

func restartMails(calls, rig string) int {
	return strings.Count(calls, "gt mail send "+rig+"/witness")
}

// A polecat whose bead is a formula wisp (gt sling) lives in the wisps table,
// which `bd list` cannot see. Its crash must still be reported.
func TestStuckAgentDogReportsCrashedPolecatOnHookedWisp(t *testing.T) {
	f := newDogFixture(t, map[string]string{"alpha": "al"})
	f.polecat("alpha", "p1")
	f.wispRows("alpha", map[string]string{"assignee": "alpha/polecats/p1", "id": "al-wisp-1", "status": "hooked"})
	f.beadStatus("al-wisp-1", "hooked")

	out, calls := f.run()
	if n := restartMails(calls, "alpha"); n != 1 {
		t.Errorf("restart mails = %d, want 1 (hooked wisp missed)\n%s", n, out)
	}
	if !strings.Contains(out, "Polecat health: 1 crashed") {
		t.Errorf("want 1 crashed:\n%s", out)
	}
}

// The hook map is read before the session check. A polecat that ran `gt done`
// in between has a closed bead and exited normally; it is not a crash.
func TestStuckAgentDogIgnoresPolecatThatFinishedAfterHookRead(t *testing.T) {
	f := newDogFixture(t, map[string]string{"alpha": "al"})
	f.polecat("alpha", "p1")
	f.listRows("alpha", "hooked", map[string]string{"assignee": "alpha/polecats/p1", "id": "al-1", "status": "hooked"})
	f.beadStatus("al-1", "closed")

	out, calls := f.run()
	if n := restartMails(calls, "alpha"); n != 0 {
		t.Errorf("restart mails = %d, want 0 for a bead closed after the hook read\n%s", n, out)
	}
	if !strings.Contains(out, "Polecat health: 0 crashed") {
		t.Errorf("want 0 crashed:\n%s", out)
	}
	if !strings.Contains(out, "completed normally") {
		t.Errorf("output should say the polecat completed normally:\n%s", out)
	}
}

// A polecat is matched only against its own rig's beads DB plus town beads,
// as `gt hook show` does. Another rig's row naming it must not count.
func TestStuckAgentDogIgnoresOtherRigsHookRows(t *testing.T) {
	f := newDogFixture(t, map[string]string{"alpha": "al", "beta": "be"})
	f.polecat("alpha", "p1")
	f.polecat("alpha", "p2")
	f.listRows("beta", "hooked", map[string]string{"assignee": "alpha/polecats/p1", "id": "be-1", "status": "hooked"})
	f.listRows("town", "hooked", map[string]string{"assignee": "alpha/polecats/p2", "id": "hq-cv-1", "status": "hooked"})
	f.beadStatus("hq-cv-1", "hooked")

	out, calls := f.run()
	if n := restartMails(calls, "alpha"); n != 1 {
		t.Errorf("restart mails = %d, want 1 (p2 via town; p1's only row is in beta)\n%s", n, out)
	}
	if strings.Contains(out, "alpha-p1") && strings.Contains(out, "CRASHED: al-p1") {
		t.Errorf("p1 reported from another rig's DB:\n%s", out)
	}
	if !strings.Contains(out, "Polecat health: 1 crashed") {
		t.Errorf("want exactly 1 crashed:\n%s", out)
	}
}

// An assignee containing a newline (or the old '|' delimiter) must not be able
// to forge a second record that names a different polecat.
func TestStuckAgentDogRejectsForgedAssigneeRecords(t *testing.T) {
	f := newDogFixture(t, map[string]string{"alpha": "al"})
	f.polecat("alpha", "p1")
	f.listRows("alpha", "hooked",
		map[string]string{"assignee": "x\nalpha/polecats/p1|al-9|hooked", "id": "al-8", "status": "hooked"},
		map[string]string{"assignee": "alpha/polecats/p1|al-7|hooked", "id": "al-6", "status": "hooked"},
	)
	f.beadStatus("al-9", "hooked")
	f.beadStatus("al-7", "hooked")

	out, calls := f.run()
	if n := restartMails(calls, "alpha"); n != 0 {
		t.Errorf("restart mails = %d, want 0 (records are forged)\n%s", n, out)
	}
	if !strings.Contains(out, "Polecat health: 0 crashed") {
		t.Errorf("want 0 crashed:\n%s", out)
	}
}
