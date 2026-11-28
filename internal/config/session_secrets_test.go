package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSessionSecretAllowlistFollowsTownConfig(t *testing.T) {
	town := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("settings/config.json", `{"type":"town-settings","version":1,"role_secrets":{"polecat":["TRACKER_TOKEN"]}}`)
	write("rig/settings/config.json", `{"merge_queue":{"reviewer_token_env":"RIG_REVIEWER_TOKEN"}}`)

	cases := []struct {
		role, name string
		want       bool
	}{
		{"rig/polecats/toast", "TRACKER_TOKEN", true},
		{"rig/witness", "TRACKER_TOKEN", false},
		{"rig/refinery", "RIG_REVIEWER_TOKEN", true},
		{"rig/crew/jane", "RIG_REVIEWER_TOKEN", true},
		{"mayor", "RIG_REVIEWER_TOKEN", true},
		{"rig/refinery", DefaultReviewerTokenEnv, false},
		{"rig/polecats/toast", "RIG_REVIEWER_TOKEN", false},
		{"rig/reviewer", "RIG_REVIEWER_TOKEN", false},
	}
	for _, tc := range cases {
		if got := slices.Contains(SessionSecretAllowlist(tc.role, town), tc.name); got != tc.want {
			t.Errorf("SessionSecretAllowlist(%q) contains %s = %v, want %v", tc.role, tc.name, got, tc.want)
		}
	}
}
