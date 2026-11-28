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

// scrubTestServer starts the shared test server and seeds its global
// environment with fixture values, as a server started from a shell holding
// them would.
func scrubTestServer(t *testing.T) *Tmux {
	t.Helper()
	tm := newTestTmux(t)
	keeper := "gt-scrub-keeper"
	_ = tm.KillSession(keeper)
	if err := tm.NewSession(keeper, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	for _, name := range scrubFixtures {
		if _, err := tm.run("set-environment", "-g", name, "fixture"); err != nil {
			t.Fatalf("set-environment %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range scrubFixtures {
			_, _ = tm.run("set-environment", "-g", "-u", name)
		}
		_ = tm.KillSession(keeper)
	})
	return tm
}

// dumpNamesCommand writes the names (never the values) of the pane's
// environment to out, then stays alive.
func dumpNamesCommand(out string) string {
	return `awk 'BEGIN{for(k in ENVIRON) print k}' > ` + out + `.tmp && mv ` + out + `.tmp ` + out + ` && sleep 30`
}

func readFixtureNames(t *testing.T, out string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(out)
		if err == nil {
			var got []string
			for _, name := range strings.Fields(string(data)) {
				if slices.Contains(scrubFixtures, name) {
					got = append(got, name)
				}
			}
			sort.Strings(got)
			return got
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

			if err := tm.NewSessionWithCommandAndEnv(session, dir, dumpNamesCommand(out), env); err != nil {
				t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
			}
			if got := readFixtureNames(t, out); !slices.Equal(got, tc.want) {
				t.Errorf("launch: secrets = %v, want %v", got, tc.want)
			}

			// A handoff or restart respawns the pane; the role comes from the
			// session environment.
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

	if err := tm.NewSessionWithCommand(session, dir, dumpNamesCommand(out)); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if got, want := readFixtureNames(t, out), []string{"ANTHROPIC_API_KEY"}; !slices.Equal(got, want) {
		t.Errorf("secrets = %v, want %v", got, want)
	}
}
