package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// SecretEnvSuffixes are the name endings that mark an environment variable as a
// secret. An agent session holds such a variable only if SessionSecretAllowlist
// names it for the session's role.
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

	switch role {
	case constants.RoleReviewer:
		// The dispatcher injects the reviewer's token under these names.
		allowed = append(allowed, githubTokenEnvs...)
	case constants.RoleRefinery, constants.RoleCrew:
		// Both run `gt reviewer request`, which reads the reviewer token.
		allowed = append(allowed, githubTokenEnvs...)
		rig, _, _ := strings.Cut(gtRole, "/")
		allowed = append(allowed, reviewerTokenEnvName(townRoot, rig))
	case constants.RoleMayor:
		allowed = append(allowed, githubTokenEnvs...)
		allowed = append(allowed, reviewerTokenEnvName(townRoot, ""))
		rigConfigs, _ := filepath.Glob(filepath.Join(townRoot, "*", "settings", "config.json"))
		for _, path := range rigConfigs {
			rig := filepath.Base(filepath.Dir(filepath.Dir(path)))
			allowed = append(allowed, reviewerTokenEnvName(townRoot, rig))
		}
	case constants.RoleAssistant:
		// The sweep's default token variables.
		allowed = append(allowed, githubTokenEnvs...)
		allowed = append(allowed, "JIRA_API_TOKEN")
	}

	if townRoot != "" && role != "" {
		if ts, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot)); err == nil && ts != nil {
			allowed = append(allowed, ts.RoleSecrets[role]...)
		}
	}

	sort.Strings(allowed)
	return slices.Compact(allowed)
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
