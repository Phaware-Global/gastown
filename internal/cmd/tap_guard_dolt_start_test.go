package cmd

import "testing"

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
