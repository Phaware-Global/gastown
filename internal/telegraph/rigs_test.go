package telegraph_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/internal/telegraph"
)

func TestLoadRigGitHubRepos_MissingFileNoError(t *testing.T) {
	t.Parallel()
	// No mayor/rigs.json is a valid state (operator may run Telegraph for
	// extra_repos only) — must not propagate as a startup failure.
	repos, err := telegraph.LoadRigGitHubRepos(t.TempDir())
	if err != nil {
		t.Fatalf("LoadRigGitHubRepos: %v, want nil", err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v, want empty", repos)
	}
}

func TestLoadRigGitHubRepos_ParsesAndDedupes(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mix of URL shapes, casing, a non-GitHub rig (should be skipped), and a
	// duplicate (should be deduped).
	rigsJSON := `{
  "version": 1,
  "rigs": {
    "gastown":           {"git_url": "https://github.com/Phaware-Global/gastown",         "added_at": "2026-04-01T00:00:00Z"},
    "android":           {"git_url": "git@github.com:Phaware-Global/heartworks-android.git", "added_at": "2026-04-01T00:00:00Z"},
    "graphql":           {"git_url": "https://github.com/Phaware-Global/graphql-api.git", "added_at": "2026-04-01T00:00:00Z"},
    "gitlab-rig":        {"git_url": "https://gitlab.com/phaware/internal",               "added_at": "2026-04-01T00:00:00Z"},
    "no-url":            {"git_url": "",                                                  "added_at": "2026-04-01T00:00:00Z"},
    "dup-by-case":       {"git_url": "https://github.com/Phaware-Global/GASTOWN",         "added_at": "2026-04-01T00:00:00Z"}
  }
}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(rigsJSON), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	repos, err := telegraph.LoadRigGitHubRepos(townRoot)
	if err != nil {
		t.Fatalf("LoadRigGitHubRepos: %v", err)
	}
	got := make([]string, len(repos))
	for i, r := range repos {
		got[i] = r
	}
	sort.Strings(got)

	want := map[string]bool{
		"Phaware-Global/gastown":            true,
		"Phaware-Global/heartworks-android": true,
		"Phaware-Global/graphql-api":        true,
	}
	if len(got) != len(want) {
		t.Errorf("got %d repos, want %d: %v", len(got), len(want), got)
	}
	for _, r := range got {
		// Case-insensitive de-dupe means "GASTOWN" and "gastown" collapse;
		// which casing wins isn't load-bearing because the translator folds
		// to lowercase for matching.
		switch r {
		case "Phaware-Global/gastown", "Phaware-Global/GASTOWN",
			"Phaware-Global/heartworks-android",
			"Phaware-Global/graphql-api":
			// ok
		default:
			t.Errorf("unexpected repo: %q", r)
		}
		_ = want
	}
}

func TestParseGitHubRepoURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string // "" means parse-rejected
	}{
		{"https_bare", "https://github.com/owner/repo", "owner/repo"},
		{"https_dotgit", "https://github.com/owner/repo.git", "owner/repo"},
		{"https_trailing_slash", "https://github.com/owner/repo/", "owner/repo"},
		{"https_uppercase", "https://github.com/Phaware-Global/gastown", "Phaware-Global/gastown"},
		{"http_legacy", "http://github.com/owner/repo", "owner/repo"},
		{"git_ssh", "git@github.com:owner/repo.git", "owner/repo"},
		{"ssh_url", "ssh://git@github.com/owner/repo.git", "owner/repo"},

		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"gitlab", "https://gitlab.com/owner/repo", ""},
		{"bitbucket_ssh", "git@bitbucket.org:owner/repo.git", ""},
		{"github_with_subpath", "https://github.com/owner/repo/tree/main", ""},
		{"missing_repo", "https://github.com/owner", ""},
		{"missing_owner", "https://github.com//repo", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := exportedParseForTest(tc.raw)
			if tc.want == "" {
				if ok {
					t.Errorf("ParseGitHubRepoURL(%q) = (%q, true); want (_, false)", tc.raw, got)
				}
				return
			}
			if !ok {
				t.Errorf("ParseGitHubRepoURL(%q) = (_, false); want (%q, true)", tc.raw, tc.want)
				return
			}
			if got != tc.want {
				t.Errorf("ParseGitHubRepoURL(%q) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// exportedParseForTest is a test-only seam — the parser stays unexported in
// the package surface so external callers can't depend on it. Round-trip
// through LoadRigGitHubRepos with a synthetic rigs.json instead, which is
// also what we do in TestLoadRigGitHubRepos_ParsesAndDedupes.
func exportedParseForTest(raw string) (string, bool) {
	townRoot, err := os.MkdirTemp("", "telegraph-rigs-test-*")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(townRoot)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		return "", false
	}
	rigsJSON, _ := json.Marshal(map[string]any{
		"version": 1,
		"rigs": map[string]any{
			"probe": map[string]any{
				"git_url":  raw,
				"added_at": "2026-04-01T00:00:00Z",
			},
		},
	})
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), rigsJSON, 0o644); err != nil {
		return "", false
	}
	repos, err := telegraph.LoadRigGitHubRepos(townRoot)
	if err != nil || len(repos) == 0 {
		return "", false
	}
	return repos[0], true
}
