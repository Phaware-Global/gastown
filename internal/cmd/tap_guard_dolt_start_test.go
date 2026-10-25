package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchesDoltServerStart(t *testing.T) {
	blocked := []string{
		"bd dolt start",
		"  bd dolt start  ",
		"bd dolt restart",
		"/Users/agent/.local/bin/bd dolt start",
		"cd /Users/agent/gt/deacon && bd dolt start",
		"cd /x; bd dolt start",
		"bd dolt status || bd dolt start",
		"(bd dolt start)",
		"echo hi\nbd dolt start",
		"nohup bd dolt start &",
		"BEADS_DIR=/x bd dolt start",
		"env FOO=1 bd dolt start",
		"bd --db /x/.beads dolt start",
		"bd -v dolt start",
		"sh -c 'bd dolt start'",
		`bash -lc "bd dolt start"`,
		"if ! gt dolt status; then bd dolt start; fi",
		"dolt sql-server",
		"dolt sql-server --port 3307",
		"nohup dolt sql-server > /tmp/dolt.log 2>&1 &",
		"cd ~/gt/.dolt-data && dolt --data-dir=. sql-server",
		"/opt/homebrew/bin/dolt sql-server",
		"sudo dolt sql-server",
		// Quotes that run code: a shell -c script and a substitution.
		`bash -c "cd /x && bd dolt start"`,
		"bash -c $'bd dolt start'",
		`sh -c 'echo "x"; dolt sql-server'`,
		`bash -c "bash -c 'bd dolt start'"`,
		`echo "$(bd dolt start)"`,
		"echo \"`bd dolt start`\"",
		`echo "ok: $(cd /x && dolt sql-server)"`,
		// Quoted data before a real start command.
		`gt mail send mayor/ -s "hi; there" && bd dolt start`,
		`bd --db "/x/.beads" dolt start`,
		`echo 'it'"'"'s' ; bd dolt start`,
		"bd create --title \"x\"\nbd dolt start",
	}
	for _, c := range blocked {
		if matchesDoltServerStart(c) == "" {
			t.Errorf("allowed %q, want blocked", c)
		}
	}

	allowed := []string{
		"",
		"gt dolt start",
		"gt dolt status",
		"gt dolt stop && gt dolt start",
		"bd dolt status",
		"bd dolt show",
		"bd list",
		"dolt sql -q 'select 1'",
		"dolt status",
		"git log --oneline",
		`bd create --title "fix dolt start hint" --type=bug`,
		`bd update gt-1 --notes "ran dolt sql-server by mistake"`,
		`gt escalate -s HIGH "Dolt: bd dolt start advice in output"`,
		`echo "do not run bd dolt start"`,
		"grep -rn 'bd dolt start' internal",
		"cat <<'EOF'\nbd dolt start\nEOF",
		"gt mail send mayor/ -s hi --body \"$(cat <<EOF\ndolt sql-server\nEOF\n)\"",
		"bd create foo <<-EOT\n\tdolt sql-server\n\tEOT\necho done",
		// Text that quotes the commands: an escalation or mail warning about them.
		`gt escalate -s HIGH "Dolt: bd said run bd dolt start"`,
		"gt escalate -s HIGH \"Dolt unreachable.\nbd dolt start\nis what bd advised\"",
		"gt mail send mayor/ -s hi -m 'agent ran; bd dolt start'",
		"gt mail send mayor/ -s hi -m $'do not\\nbd dolt start'",
		`bd create --title "x && bd dolt start && y" --type=bug`,
		`bd create -d "then bd dolt start | dolt sql-server"`,
		`gt nudge deacon "never run (bd dolt start) or dolt sql-server"`,
		`gt escalate "it's down; bd dolt start" && gt dolt status`,
		`echo "$(gt dolt status) mentions bd dolt start"`,
		`grep -c "bd dolt start" notes.txt`,
		`bash -c "gt dolt status" 'bd dolt start'`,
	}
	for _, c := range allowed {
		if reason := matchesDoltServerStart(c); reason != "" {
			t.Errorf("blocked %q (%s), want allowed", c, reason)
		}
	}
}

// A heredoc hides only its own body: a start command after the terminator is
// still a start command.
func TestMatchesDoltServerStart_CommandAfterHeredoc(t *testing.T) {
	cmd := "cat <<'EOF'\nnotes\nEOF\nbd dolt start"
	if matchesDoltServerStart(cmd) == "" {
		t.Errorf("allowed %q, want blocked", cmd)
	}
}

func TestTapGuardDoltStart_OnlyInAgentContext(t *testing.T) {
	for _, v := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS", "GT_REFINERY", "GT_MAYOR", "GT_DEACON", "GT_ROLE", "GT_RIG", "GT_TOWN_ROOT"} {
		t.Setenv(v, "")
	}
	t.Chdir(t.TempDir())
	if isGasTownAgentContext() {
		t.Skip("scratch directory resolves to a Gas Town agent context")
	}
	run := func(command string) error {
		defer setStdinForTest(t, `{"tool_name":"Bash","tool_input":{"command":"`+command+`"}}`)()
		return runTapGuardDoltStart(tapGuardDoltStartCmd, nil)
	}

	if err := run("bd dolt start"); err != nil {
		t.Errorf("guard fired outside an agent context: %v", err)
	}

	t.Setenv("GT_ROLE", "deacon")
	if code, ok := IsSilentExit(run("bd dolt start")); !ok || code != 2 {
		t.Errorf("bd dolt start in agent context: want silent exit 2")
	}
	if err := run("gt dolt status"); err != nil {
		t.Errorf("guard blocked gt dolt status: %v", err)
	}
}

// Unclosed "$( runs make the quote scanner backtrack exponentially. The guard
// must still return within its budget, and let the command run.
func TestTapGuardDoltStart_FailsOpenWhenMatcherIsSlow(t *testing.T) {
	for _, v := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS", "GT_REFINERY", "GT_MAYOR", "GT_DEACON", "GT_ROLE", "GT_RIG", "GT_TOWN_ROOT"} {
		t.Setenv(v, "")
	}
	t.Chdir(t.TempDir())
	t.Setenv("GT_ROLE", "deacon")

	adversarial := "echo " + strings.Repeat(`"$(`, 32) + "\nbd dolt start"
	payload, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": adversarial},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer setStdinForTest(t, string(payload))()

	start := time.Now()
	err = runTapGuardDoltStart(tapGuardDoltStartCmd, nil)
	if elapsed := time.Since(start); elapsed > 5*doltStartMatchBudget {
		t.Errorf("guard took %v, want about %v", elapsed, doltStartMatchBudget)
	}
	if err != nil {
		t.Errorf("guard blocked on timeout, want fail-open: %v", err)
	}
}

// The opencode plugin strips the shared service's session identity (GT_ROLE,
// GT_RIG, ...) before running gt, and the deacon and dogs live at <town>/deacon
// and <town>/deacon/dogs/<name>, which the path fallback does not cover. The
// plugin keeps GT_TOWN_ROOT so the guard still knows it is in an agent session.
func TestTapGuardDoltStart_OpenCodeScrubbedEnv(t *testing.T) {
	for _, v := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS", "GT_REFINERY", "GT_MAYOR", "GT_DEACON", "GT_ROLE", "GT_RIG", "GT_TOWN_ROOT"} {
		t.Setenv(v, "")
	}
	town := t.TempDir()
	deacon := filepath.Join(town, "deacon")
	if err := os.MkdirAll(deacon, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deacon)
	t.Setenv("GT_ROOT", town)
	if isGasTownAgentContext() {
		t.Fatal("GT_ROOT alone must not mark agent context; the plugin relies on GT_TOWN_ROOT for that")
	}
	t.Setenv("GT_TOWN_ROOT", town)
	if !isGasTownAgentContext() {
		t.Fatal("GT_TOWN_ROOT from a deacon-shaped cwd must mark agent context, or the plugin's guard call is a no-op")
	}
}

func TestMatchesDoltServerStartWithin(t *testing.T) {
	if reason, ok := matchesDoltServerStartWithin("bd dolt start", time.Second); !ok || reason == "" {
		t.Errorf("real start: reason=%q ok=%v, want a verdict", reason, ok)
	}
	if reason, ok := matchesDoltServerStartWithin("gt dolt status", time.Second); !ok || reason != "" {
		t.Errorf("benign: reason=%q ok=%v, want allowed", reason, ok)
	}
}
