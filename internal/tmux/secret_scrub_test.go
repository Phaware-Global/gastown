//go:build !windows

package tmux

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The three secrets of the town services, plus one credential of each other
// kind the allowlist distinguishes.
var scrubFixtures = []string{
	"ANTHROPIC_API_KEY",
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GT_REVIEWER_GITHUB_TOKEN",
	"GT_TELEGRAPH_GITHUB_SECRET",
	"GT_TELEGRAPH_JIRA_SECRET",
	"UNRELATED_DEPLOY_PASSWORD",
	"UNRELATED_SERVICE_PAT",
}

// scrubRoles stands in for gt's session naming, which this package cannot see.
var scrubRoles = map[string]string{}

// scrubTestServer starts the shared test server and seeds its global
// environment with fixture values, as a server started from a shell holding
// them would.
func scrubTestServer(t *testing.T) *Tmux {
	t.Helper()
	tm := newTestTmux(t)
	town := t.TempDir()
	t.Setenv("GT_TOWN_ROOT", town)
	t.Setenv("GT_ROOT", town)
	previous := sessionRole
	scrubRoles = map[string]string{}
	SetSessionRoleResolver(func(session string) (string, bool) {
		role, ok := scrubRoles[session]
		return role, ok
	})
	t.Cleanup(func() { sessionRole = previous })
	keeper := "gt-scrub-keeper"
	_ = tm.KillSession(keeper)
	if err := tm.NewSession(keeper, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	seedGlobal(t, tm, scrubFixtures...)
	t.Cleanup(func() {
		for _, name := range scrubFixtures {
			_, _ = tm.run("set-environment", "-g", "-u", name)
		}
		_ = tm.KillSession(keeper)
	})
	return tm
}

func seedGlobal(t *testing.T, tm *Tmux, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := tm.run("set-environment", "-g", name, "fixture"); err != nil {
			t.Fatalf("set-environment %s: %v", name, err)
		}
	}
}

// listedNames returns which of names show-environment reports as set, in the
// global environment (session "") or a session's.
func listedNames(t *testing.T, tm *Tmux, session string, names []string) []string {
	t.Helper()
	args := []string{"show-environment", "-g"}
	if session != "" {
		args = []string{"show-environment", "-t", session}
	}
	out, err := tm.run(args...)
	if err != nil {
		t.Fatalf("show-environment %q: %v", session, err)
	}
	var got []string
	for _, line := range strings.Split(out, "\n") {
		name, _, ok := strings.Cut(line, "=")
		if ok && slices.Contains(names, name) {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	return got
}

// dumpNamesCommand writes the names (never the values) of the pane's
// environment to out, then stays alive.
func dumpNamesCommand(out string) string {
	return `awk 'BEGIN{for(k in ENVIRON) print k}' > ` + out + `.tmp && mv ` + out + `.tmp ` + out + ` && sleep 30`
}

func readFixtureNames(t *testing.T, out string) []string {
	t.Helper()
	var got []string
	for _, name := range strings.Fields(waitForFile(t, out)) {
		if slices.Contains(scrubFixtures, name) {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	return got
}

func waitForFile(t *testing.T, out string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(out)
		if err == nil {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never wrote %s: %v", out, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSessionEnvHoldsExactlyTheRoleSecrets(t *testing.T) {
	tm := scrubTestServer(t)

	cases := []struct {
		role string
		want []string
	}{
		{"rig/polecats/toast", []string{"ANTHROPIC_API_KEY"}},
		{"rig/witness", []string{"ANTHROPIC_API_KEY"}},
		{"deacon", []string{"ANTHROPIC_API_KEY"}},
		{"deacon/boot", []string{"ANTHROPIC_API_KEY"}},
		{"dog", []string{"ANTHROPIC_API_KEY"}},
		{"", []string{"ANTHROPIC_API_KEY"}},
		{"rig/reviewer", []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN"}},
		{"rig/refinery", []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "GT_REVIEWER_GITHUB_TOKEN"}},
		{"rig/crew/jane", []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "GT_REVIEWER_GITHUB_TOKEN"}},
		{"mayor", []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "GT_REVIEWER_GITHUB_TOKEN"}},
		{"assistant", []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN"}},
	}

	for i, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			dir := t.TempDir()
			session := "gt-scrub-" + string(rune('a'+i))
			out := filepath.Join(dir, "names")
			env := map[string]string{"GT_ROOT": dir}
			if tc.role != "" {
				env["GT_ROLE"] = tc.role
			}
			_ = tm.KillSession(session)
			t.Cleanup(func() { _ = tm.KillSession(session) })

			seedGlobal(t, tm, scrubFixtures...)
			scrubRoles[session] = tc.role
			if err := tm.NewSessionWithCommandAndEnv(session, dir, dumpNamesCommand(out), env); err != nil {
				t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
			}
			if got := readFixtureNames(t, out); !slices.Equal(got, tc.want) {
				t.Errorf("launch: secrets = %v, want %v", got, tc.want)
			}

			// A handoff or restart respawns the pane; the role comes from the
			// session's name.
			for name, respawn := range map[string]func(string) error{
				"respawn": func(cmd string) error { return tm.RespawnPane(session, cmd) },
				"respawn-workdir": func(cmd string) error {
					return tm.RespawnPaneWithWorkDir(session, dir, cmd)
				},
			} {
				again := filepath.Join(dir, name)
				if err := respawn(dumpNamesCommand(again)); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if got := readFixtureNames(t, again); !slices.Equal(got, tc.want) {
					t.Errorf("%s: secrets = %v, want %v", name, got, tc.want)
				}
			}
		})
	}
}

func TestNewSessionWithCommandDropsInheritedSecrets(t *testing.T) {
	tm := scrubTestServer(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "names")
	session := "gt-scrub-plain"
	_ = tm.KillSession(session)
	t.Cleanup(func() { _ = tm.KillSession(session) })

	seedGlobal(t, tm, scrubFixtures...)
	if err := tm.NewSessionWithCommand(session, dir, dumpNamesCommand(out)); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if got, want := readFixtureNames(t, out), []string{"ANTHROPIC_API_KEY"}; !slices.Equal(got, want) {
		t.Errorf("secrets = %v, want %v", got, want)
	}
}

func startScrubSession(t *testing.T, tm *Tmux, session, role string, env map[string]string) {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	env["GT_ROLE"] = role
	scrubRoles[session] = role
	if env["GT_ROOT"] == "" {
		env["GT_ROOT"] = t.TempDir()
	}
	_ = tm.KillSession(session)
	t.Cleanup(func() { _ = tm.KillSession(session) })
	if err := tm.NewSessionWithCommandAndEnv(session, env["GT_ROOT"], "sleep 30", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
}

func TestSessionStartEmptiesGlobalEnvOfSecrets(t *testing.T) {
	tm := scrubTestServer(t)

	startScrubSession(t, tm, "gt-scrub-reviewer", "rig/reviewer", map[string]string{"GH_TOKEN": "dispatched"})
	seedGlobal(t, tm, scrubFixtures...)
	startScrubSession(t, tm, "gt-scrub-polecat", "rig/polecats/toast", map[string]string{"UNRELATED_SERVICE_PAT": "caller"})

	if got := listedNames(t, tm, "", scrubFixtures); len(got) != 0 {
		t.Errorf("global environment still holds %v", got)
	}
	if got, want := listedNames(t, tm, "gt-scrub-polecat", scrubFixtures), []string{"ANTHROPIC_API_KEY"}; !slices.Equal(got, want) {
		t.Errorf("polecat session secrets = %v, want %v", got, want)
	}
	want := []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN"}
	if got := listedNames(t, tm, "gt-scrub-reviewer", scrubFixtures); !slices.Equal(got, want) {
		t.Errorf("reviewer session secrets = %v, want %v", got, want)
	}
	if got, err := tm.GetEnvironment("gt-scrub-reviewer", "GH_TOKEN"); err != nil || got != "dispatched" {
		t.Errorf("reviewer lost the token it was dispatched with (err=%v)", err)
	}
}

// A session that was inheriting a secret from the global environment keeps it
// when that environment is emptied.
func TestWithheldSecretsStayWithRunningSessions(t *testing.T) {
	tm := scrubTestServer(t)
	startScrubSession(t, tm, "gt-scrub-refinery", "rig/refinery", nil)
	// Return the session to the state of one that only inherits.
	for _, name := range scrubFixtures {
		_, _ = tm.run("set-environment", "-t", "gt-scrub-refinery", "-u", name)
	}
	seedGlobal(t, tm, scrubFixtures...)
	startScrubSession(t, tm, "gt-scrub-other", "rig/witness", nil)

	out := filepath.Join(t.TempDir(), "names")
	if err := tm.RespawnPane("gt-scrub-refinery", dumpNamesCommand(out)); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
	want := []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "GT_REVIEWER_GITHUB_TOKEN"}
	if got := readFixtureNames(t, out); !slices.Equal(got, want) {
		t.Errorf("respawned refinery secrets = %v, want %v", got, want)
	}
}

// With nothing in the global environment, a session gets its secrets from the
// process that starts it.
func TestSessionSecretsComeFromStartingProcess(t *testing.T) {
	tm := scrubTestServer(t)
	for _, name := range scrubFixtures {
		_, _ = tm.run("set-environment", "-g", "-u", name)
		t.Setenv(name, "fixture")
	}
	startScrubSession(t, tm, "gt-scrub-own", "rig/refinery", nil)

	want := []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "GT_REVIEWER_GITHUB_TOKEN"}
	if got := listedNames(t, tm, "gt-scrub-own", scrubFixtures); !slices.Equal(got, want) {
		t.Errorf("session secrets = %v, want %v", got, want)
	}
}

func TestConfiguredSecretNamesNeedNoSuffix(t *testing.T) {
	tm := scrubTestServer(t)
	town := t.TempDir()
	t.Setenv("GT_TOWN_ROOT", town)
	for rel, body := range map[string]string{
		"settings/config.json":     `{"type":"town-settings","version":1,"assistant":{"sweep":{"jira":{"token_env":"ACME_JIRA_CRED"},"github":{"token_env":"ACME_FORGE_CRED"}}},"role_secrets":{"witness":["ACME_EXTRA_CRED"]}}`,
		"settings/telegraph.toml":  "[telegraph.providers.github]\nsecret_env = \"ACME_WEBHOOK_HMAC\"\n",
		"rig/settings/config.json": `{"merge_queue":{"reviewer_token_env":"ACME_REVIEWER_CRED"}}`,
	} {
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"ACME_EXTRA_CRED", "ACME_FORGE_CRED", "ACME_JIRA_CRED", "ACME_REVIEWER_CRED", "ACME_WEBHOOK_HMAC"}
	seedGlobal(t, tm, names...)
	t.Cleanup(func() {
		for _, name := range names {
			_, _ = tm.run("set-environment", "-g", "-u", name)
		}
	})

	startScrubSession(t, tm, "gt-scrub-cfg", "rig/polecats/toast", map[string]string{"GT_ROOT": town})
	if got := listedNames(t, tm, "", names); len(got) != 0 {
		t.Errorf("global environment still holds %v", got)
	}
	if got := listedNames(t, tm, "gt-scrub-cfg", names); len(got) != 0 {
		t.Errorf("polecat session holds %v", got)
	}

	// The pane drops them even when they reach it some other way.
	for _, name := range names {
		if err := tm.SetEnvironment("gt-scrub-cfg", name, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "names")
	if err := tm.RespawnPane("gt-scrub-cfg", dumpNamesCommand(out)); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
	data := waitForFile(t, out)
	for _, name := range names {
		if slices.Contains(strings.Fields(data), name) {
			t.Errorf("pane holds %s", name)
		}
	}
}

// A session started by an agent, whose own environment is scrubbed, still gets
// its role's secrets from the ones the server withholds.
func TestScrubbedStarterStillGivesSessionItsSecrets(t *testing.T) {
	tm := scrubTestServer(t)
	for _, name := range scrubFixtures {
		t.Setenv(name, "") // restores the variable after the test
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	// The first start withholds the secrets and kills nothing that holds them.
	startScrubSession(t, tm, "gt-scrub-first", "rig/polecats/toast", nil)

	dir := t.TempDir()
	out := filepath.Join(dir, "names")
	session := "gt-scrub-restarted"
	_ = tm.KillSession(session)
	t.Cleanup(func() { _ = tm.KillSession(session) })
	env := map[string]string{"GT_ROLE": "rig/refinery", "GT_ROOT": dir}
	scrubRoles[session] = "rig/refinery"
	if err := tm.NewSessionWithCommandAndEnv(session, dir, dumpNamesCommand(out), env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}

	want := []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "GT_REVIEWER_GITHUB_TOKEN"}
	if got := readFixtureNames(t, out); !slices.Equal(got, want) {
		t.Errorf("refinery pane secrets = %v, want %v", got, want)
	}
	if got := listedNames(t, tm, "", scrubFixtures); len(got) != 0 {
		t.Errorf("show-environment -g prints %v", got)
	}
	if got, want := listedNames(t, tm, "gt-scrub-first", scrubFixtures), []string{"ANTHROPIC_API_KEY"}; !slices.Equal(got, want) {
		t.Errorf("polecat session secrets = %v, want %v", got, want)
	}
}
