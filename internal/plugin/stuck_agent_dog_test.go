package plugin

import (
	"context"
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
// `gt hook show` and `bd show` sleep like the real commands do after a
// town-wide down (~5s each), so a regression to per-polecat lookups blows
// the test's deadline as well as its call-count assertions.
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
  show) sleep 5; echo '[{"status":"hooked"}]' ;;
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
	if n := count("bd show"); n != 0 {
		t.Errorf("run.sh spawned `bd show` %d times; status comes from the batched list", n)
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
