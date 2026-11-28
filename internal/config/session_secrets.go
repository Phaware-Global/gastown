package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/constants"
)

// SecretEnvSuffixes are the name endings that mark an environment variable as a
// secret, as are the names ConfiguredSecretEnvNames returns. An agent session
// holds such a variable only if SessionSecretAllowlist names it for the
// session's role.
var SecretEnvSuffixes = []string{"_TOKEN", "_SECRET", "_PAT", "_KEY", "_PASSWORD"}

// baseSessionSecrets are held by every session: the agent cannot reach its
// model provider, and bd cannot reach Dolt, without them.
var baseSessionSecrets = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_FOUNDRY_API_KEY",
	"AWS_BEARER_TOKEN_BEDROCK",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"CLAUDE_CODE_CLIENT_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GROQ_API_KEY",
	"OPENAI_API_KEY",
	"BEADS_DOLT_PASSWORD",
	"DOLT_CLI_PASSWORD",
	"GT_DOLT_PASSWORD",
	"GT_PROXY_KEY", // path to the proxy client's key file
}

var githubTokenEnvs = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// SessionSecretAllowlist returns the sorted names of the secret environment
// variables a session with the given GT_ROLE may hold. An empty or unknown role
// gets only the base set. townRoot may be empty; when set, it supplies the
// configured reviewer token name and the town's role_secrets additions.
//
// Telegraph is not an agent session, so no role here holds its secrets.
func SessionSecretAllowlist(gtRole, townRoot string) []string {
	role := ExtractSimpleRole(gtRole)
	allowed := slices.Clone(baseSessionSecrets)

	var ts *TownSettings
	if townRoot != "" {
		ts = loadTownSettingsIfValid(townRoot)
	}

	switch role {
	case constants.RolePolecat:
		// A remote polecat's pane runs `gt worker-attach`, which
		// authenticates to the worker with this.
		allowed = append(allowed, "GT_WORKER_TOKEN")
	case constants.RoleReviewer:
		// The dispatcher injects the reviewer's token under these names.
		allowed = append(allowed, githubTokenEnvs...)
	case constants.RoleRefinery, constants.RoleCrew:
		// Both run `gt reviewer request`, which reads the reviewer token.
		allowed = append(allowed, githubTokenEnvs...)
		rig, _, _ := strings.Cut(gtRole, "/")
		allowed = append(allowed, reviewerTokenEnvName(townRoot, rig))
		if role == constants.RoleRefinery {
			// The merge queue's PR provider on a Bitbucket rig.
			allowed = append(allowed, "BITBUCKET_TOKEN")
		}
	case constants.RoleMayor:
		allowed = append(allowed, githubTokenEnvs...)
		allowed = append(allowed, reviewerTokenEnvName(townRoot, ""))
		for _, rig := range rigsWithSettings(townRoot) {
			allowed = append(allowed, reviewerTokenEnvName(townRoot, rig))
		}
	case constants.RoleAssistant:
		// The sweep's default and configured token variables.
		allowed = append(allowed, githubTokenEnvs...)
		allowed = append(allowed, "JIRA_API_TOKEN", ts.JiraSweep().TokenEnv, ts.GitHubSweep().TokenEnv)
	}

	if ts != nil && role != "" {
		allowed = append(allowed, ts.RoleSecrets[role]...)
	}

	return sortedNames(allowed)
}

// ConfiguredSecretEnvNames returns the sorted names of the environment
// variables the town's config says hold secrets, whatever they end in: reviewer
// tokens, telegraph webhook secrets, assistant sweep tokens and role_secrets.
func ConfiguredSecretEnvNames(townRoot string) []string {
	if townRoot == "" {
		return nil
	}
	names := []string{DefaultReviewerTokenEnv}
	for _, rig := range rigsWithSettings(townRoot) {
		names = append(names, reviewerTokenEnvName(townRoot, rig))
	}
	if ts := loadTownSettingsIfValid(townRoot); ts != nil {
		names = append(names, ts.JiraSweep().TokenEnv, ts.GitHubSweep().TokenEnv)
		for _, extra := range ts.RoleSecrets {
			names = append(names, extra...)
		}
	}
	// internal/telegraph owns this file; only the secret names are read here.
	var telegraph struct {
		Telegraph struct {
			Providers map[string]struct {
				SecretEnv string `toml:"secret_env"`
			} `toml:"providers"`
		} `toml:"telegraph"`
	}
	if _, err := toml.DecodeFile(filepath.Join(townRoot, "settings", "telegraph.toml"), &telegraph); err == nil {
		for _, provider := range telegraph.Telegraph.Providers {
			names = append(names, provider.SecretEnv)
		}
	}
	return sortedNames(names)
}

// IsSecretEnvName reports whether name holds a secret: it ends in one of
// SecretEnvSuffixes or is one of configured.
func IsSecretEnvName(name string, configured []string) bool {
	upper := strings.ToUpper(name)
	return slices.Contains(configured, name) ||
		slices.ContainsFunc(SecretEnvSuffixes, func(suffix string) bool { return strings.HasSuffix(upper, suffix) })
}

func sortedNames(names []string) []string {
	names = slices.DeleteFunc(names, func(name string) bool { return name == "" })
	sort.Strings(names)
	return slices.Compact(names)
}

func loadTownSettingsIfValid(townRoot string) *TownSettings {
	ts, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		return nil
	}
	return ts
}

func rigsWithSettings(townRoot string) []string {
	paths, _ := filepath.Glob(filepath.Join(townRoot, "*", "settings", "config.json"))
	rigs := make([]string, 0, len(paths))
	for _, path := range paths {
		rigs = append(rigs, filepath.Base(filepath.Dir(filepath.Dir(path))))
	}
	return rigs
}

// reviewerTokenEnvName returns the rig's merge_queue.reviewer_token_env, or the
// default. It reads the same files, in the same order, as the refinery.
func reviewerTokenEnvName(townRoot, rig string) string {
	if townRoot == "" || rig == "" {
		return DefaultReviewerTokenEnv
	}
	for _, path := range []string{
		filepath.Join(townRoot, rig, "settings", "config.json"),
		filepath.Join(townRoot, rig, "config.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file struct {
			MergeQueue *MergeQueueConfig `json:"merge_queue"`
		}
		if json.Unmarshal(data, &file) == nil && file.MergeQueue != nil {
			return file.MergeQueue.GetReviewerTokenEnv()
		}
	}
	return DefaultReviewerTokenEnv
}
