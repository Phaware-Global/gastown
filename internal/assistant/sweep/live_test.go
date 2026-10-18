//go:build live

package sweep

// Live integration tests run one port against a real town and the real
// external system. They are run by hand, never in CI: they need the "live"
// build tag and GT_SWEEP_LIVE_TOWN. Credentials come from the environment
// exactly as in production (JIRA_API_TOKEN or assistant.sweep.jira.token_env;
// GITHUB_TOKEN or `gh auth token`). The ports are called directly, so
// nothing is recorded in the sweep state. The rendered report is written to
// GT_SWEEP_LIVE_OUT (default: the test log) for a human to compare with what
// the system shows right now; the test itself checks only invariants: the
// port returns, every finding already satisfies the port's declared schema
// and Suggest templates, and no two findings share an ID.
//
//	GT_SWEEP_LIVE_TOWN=$HOME/gt GT_SWEEP_LIVE_OUT=/tmp/sweep-jira.md \
//	  go test -tags live -run TestLive -v ./internal/assistant/sweep/
//
// GT_SWEEP_LIVE_SETTINGS points at an alternative settings file (same schema
// as settings/config.json) when the town's own file has no assistant.sweep
// section yet.

import (
	"context"
	"errors"
	"maps"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

func liveInput(t *testing.T) Input {
	t.Helper()
	town := os.Getenv("GT_SWEEP_LIVE_TOWN")
	if town == "" {
		t.Skip("set GT_SWEEP_LIVE_TOWN to run against a live town")
	}
	path := os.Getenv("GT_SWEEP_LIVE_SETTINGS")
	if path == "" {
		path = config.TownSettingsPath(town)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings, err := config.LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	return Input{TownRoot: town, Settings: settings, Now: time.Now()}
}

// liveSweep runs the port the way the runner does (under PortTimeout),
// checks the invariants, and renders the report.
func liveSweep(t *testing.T, port Port, in Input) []Finding {
	t.Helper()
	start := time.Now()
	findings, err := runPort(context.Background(), port, in)
	res := PortResult{Name: port.Name(), New: findings}
	var partial *PartialError
	switch {
	case errors.As(err, &partial):
		res.Partial = true
		t.Logf("PARTIAL: %s", partial.Reason)
	case err != nil:
		res.Error = err.Error()
		t.Errorf("port %s failed: %v", port.Name(), err)
	}
	t.Logf("%s: %d findings in %s", port.Name(), len(findings), time.Since(start).Round(time.Millisecond))

	schema, templates := port.Schema(), port.Suggestions()
	ids := map[string]bool{}
	byKind := map[string]int{}
	for _, f := range findings {
		byKind[f.Kind]++
		id := f.Kind + "/" + f.Key
		if ids[id] {
			t.Errorf("duplicate finding ID %s", id)
		}
		ids[id] = true
		if f.Since.After(in.Now.Add(time.Minute)) {
			t.Errorf("%s: Since %s is in the future", id, f.Since)
		}
		// The port must emit findings that already pass its schema; a value
		// the runner would have to move into the fence is a port bug.
		g := f
		g.Fields, g.External = maps.Clone(f.Fields), maps.Clone(f.External)
		enforceSchema(schema, templates, &g)
		if !reflect.DeepEqual(f, g) {
			t.Errorf("%s: schema enforcement changed the finding\n  port:   %+v\n  shaped: %+v", id, f, g)
		}
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		t.Logf("  %-24s %d", k, byKind[k])
	}

	report := Markdown(&Report{RunID: "live-" + in.Now.UTC().Format("20060102T150405Z"), GeneratedAt: in.Now, Ports: []PortResult{res}})
	if out := os.Getenv("GT_SWEEP_LIVE_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report), 0o600); err != nil {
			t.Fatalf("writing report: %v", err)
		}
		t.Logf("report written to %s", out)
	} else {
		t.Logf("report:\n%s", report)
	}
	return findings
}

func TestLiveJira(t *testing.T) {
	in := liveInput(t)
	port := NewJiraPort(in.TownRoot, os.Getenv)
	cfg := in.Settings.JiraSweep()
	if cfg.Site == "" || len(cfg.Projects) == 0 {
		t.Skip("assistant.sweep.jira.site and .projects are not configured")
	}
	t.Logf("jira: site=%s projects=%s board=%d", cfg.Site, strings.Join(cfg.Projects, ","), cfg.BoardID)
	findings := liveSweep(t, port, in)
	for _, f := range findings {
		if f.URL != "" && !strings.HasPrefix(f.URL, "https://") {
			t.Errorf("%s/%s: URL %q is not the issue's browse URL", f.Kind, f.Key, f.URL)
		}
	}
}

func TestLiveGitHub(t *testing.T) {
	in := liveInput(t)
	port := NewGitHubPort(in.TownRoot, os.Getenv)
	repos, err := port.repos()
	if err != nil {
		t.Fatalf("rig repos: %v", err)
	}
	logins, extra, err := port.persona()
	if err != nil {
		t.Fatalf("persona: %v", err)
	}
	slugs := make([]string, 0, len(repos))
	for _, r := range repos {
		slugs = append(slugs, r.slug())
	}
	t.Logf("github: persona=%s rigs=%s extra=%s", strings.Join(logins, ","), strings.Join(slugs, ","), strings.Join(append(extra, in.Settings.GitHubSweep().ExtraRepos...), ","))
	findings := liveSweep(t, port, in)
	for _, f := range findings {
		if !strings.HasPrefix(f.URL, "https://github.com/") {
			t.Errorf("%s/%s: URL %q is not a GitHub URL", f.Kind, f.Key, f.URL)
		}
	}
}
