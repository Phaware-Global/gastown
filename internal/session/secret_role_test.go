//go:build !windows

package session

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

var secretRoleFixtures = []string{"ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "UNRELATED_DEPLOY_PASSWORD"}

// secretRoleServer starts a private tmux server and registers the rig prefix
// "zz". Fixture variables are unset in this process until a test sets them.
func secretRoleServer(t *testing.T) (*tmux.Tmux, func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	for _, name := range secretRoleFixtures {
		t.Setenv(name, "") // restores the variable after the test
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	town := t.TempDir()
	t.Setenv("GT_TOWN_ROOT", town)
	t.Setenv("GT_ROOT", town)

	old := DefaultRegistry()
	registry := NewPrefixRegistry()
	registry.Register("zz", "rig")
	SetDefaultRegistry(registry)
	t.Cleanup(func() { SetDefaultRegistry(old) })

	server := fmt.Sprintf("gt-test-role-%d", os.Getpid())
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-L", server}, args...)...).Output()
		if err != nil {
			t.Fatalf("tmux %s: %v", args[0], err) // arguments may hold values
		}
		return string(out)
	}
	run("new-session", "-d", "-s", "keeper", "sleep 60")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", server, "kill-server").Run() })
	return tmux.NewTmuxWithSocket(server), run
}

// presentFixtures returns the fixture names a show-environment listing reports
// as set. Values are discarded.
func presentFixtures(listing string) []string {
	var got []string
	for _, line := range strings.Split(listing, "\n") {
		name, _, set := strings.Cut(line, "=")
		if set && slices.Contains(secretRoleFixtures, name) {
			got = append(got, name)
		}
	}
	slices.Sort(got)
	return got
}

func TestRelabelledSessionGetsNoOtherRolesSecrets(t *testing.T) {
	tm, run := secretRoleServer(t)
	run("new-session", "-d", "-s", "zz-toast", "sleep 60")
	run("set-environment", "-t", "zz-toast", "GT_ROLE", "mayor")

	// The next start or attach, by a process holding forge tokens, with a bait
	// secret in the global environment.
	t.Setenv("GH_TOKEN", "fixture")
	run("set-environment", "-g", "GITHUB_TOKEN", "fixture")
	tm.WithholdGlobalSecrets("")

	assertNone := func(when string) {
		t.Helper()
		if got := presentFixtures(run("show-environment", "-t", "zz-toast")); len(got) != 0 {
			t.Errorf("%s: polecat session holds %v", when, got)
		}
	}
	assertNone("withhold")
	if err := tm.RespawnPane("zz-toast", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	assertNone("respawn")
}

func TestUnknownSessionGetsNoSecrets(t *testing.T) {
	tm, run := secretRoleServer(t)
	run("new-session", "-d", "-s", "stranger", "sleep 60")
	run("set-environment", "-t", "stranger", "GT_ROLE", "mayor")
	t.Setenv("GH_TOKEN", "fixture")
	run("set-environment", "-g", "GITHUB_TOKEN", "fixture")
	run("set-environment", "-g", "ANTHROPIC_API_KEY", "fixture")

	tm.WithholdGlobalSecrets("")
	if err := tm.RespawnPane("stranger", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if got := presentFixtures(run("show-environment", "-t", "stranger")); len(got) != 0 {
		t.Errorf("unknown session holds %v", got)
	}
}

// A running session gets its role's secrets from the ones the server held.
func TestNamedSessionKeepsServerHeldSecrets(t *testing.T) {
	tm, run := secretRoleServer(t)
	run("new-session", "-d", "-s", "zz-reviewer", "sleep 60")
	run("new-session", "-d", "-s", "hq-mayor", "sleep 60")
	run("set-environment", "-g", "GH_TOKEN", "fixture")

	tm.WithholdGlobalSecrets("")
	for _, session := range []string{"zz-reviewer", "hq-mayor"} {
		if got := presentFixtures(run("show-environment", "-t", session)); !slices.Equal(got, []string{"GH_TOKEN"}) {
			t.Errorf("%s holds %v", session, got)
		}
	}
}

func TestSecretNoRoleMayHoldLeavesTheServer(t *testing.T) {
	tm, run := secretRoleServer(t)
	run("set-environment", "-g", "UNRELATED_DEPLOY_PASSWORD", "fixture")
	run("set-environment", "-g", "GH_TOKEN", "fixture")

	town := os.Getenv("GT_ROOT")
	env := map[string]string{"GT_ROLE": "rig/polecats/toast", "GT_ROOT": town}
	if err := tm.NewSessionWithCommandAndEnv("zz-toast", town, "sleep 60", env); err != nil {
		t.Fatal(err)
	}
	if got := presentFixtures(run("show-environment", "-g")); len(got) != 0 {
		t.Errorf("show-environment -g prints %v", got)
	}
	// GH_TOKEN is on some role's allowlist, so its hidden copy stays.
	if got := presentFixtures(run("show-environment", "-gh")); !slices.Equal(got, []string{"GH_TOKEN"}) {
		t.Errorf("show-environment -gh prints %v", got)
	}
}

// The role looked up for a session is the one its agent was started with.
func TestSecretRoleForSessionMatchesAgentEnv(t *testing.T) {
	secretRoleServer(t)
	for session, cfg := range map[string]config.AgentEnvConfig{
		"hq-mayor":      {Role: "mayor"},
		"hq-assistant":  {Role: "assistant"},
		"hq-deacon":     {Role: "deacon"},
		"hq-boot":       {Role: "boot"},
		"hq-dog-alpha":  {Role: "dog", AgentName: "alpha"},
		"zz-witness":    {Role: "witness", Rig: "rig"},
		"zz-refinery":   {Role: "refinery", Rig: "rig"},
		"zz-reviewer":   {Role: "reviewer", Rig: "rig"},
		"zz-crew-jane":  {Role: "crew", Rig: "rig", AgentName: "jane"},
		"zz-toast":      {Role: "polecat", Rig: "rig", AgentName: "toast"},
		"hq-overseer":   {},
		"stranger":      {},
		"other-session": {},
	} {
		want := config.AgentEnv(cfg)["GT_ROLE"]
		if got, ok := secretRoleForSession(session); got != want || ok != (want != "") {
			t.Errorf("%s: role %q (known=%v), want %q", session, got, ok, want)
		}
	}
}
