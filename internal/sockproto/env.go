package sockproto

import "strings"

// Session-env policy for the exec stream (§4.3 attach, core §7.4). It lives
// here, on the wire package, so BOTH sides share one definition: the launcher
// filters its own environment down to this set, and the worker re-validates
// against the SAME set before anything reaches the agent. Two lists documented
// to agree would drift — and a drift means either a silently dropped session
// var or an attach the worker refuses.
//
// It is an ALLOWLIST, not a denylist. The worker's check is a confused-deputy /
// compromised-orchestrator guard, and a denylist cannot enumerate what must be
// refused: loader vars (LD_PRELOAD, DYLD_INSERT_LIBRARIES) would load
// attacker-chosen code into a NATIVE agent running on the worker host, and PATH
// would hijack every subprocess it spawns.
var (
	// envAllowedPrefixes are families forwarded wholesale: gastown's own session
	// vars, beads, and the model-config carry-over.
	envAllowedPrefixes = []string{"GT_", "BD_", "ANTHROPIC_DEFAULT_"}
	// envAllowedExact are individually permitted keys. They are model/mode
	// SELECTION only — nothing here can change where a credential is sent.
	//
	// ANTHROPIC_BASE_URL is deliberately absent. It is not itself a secret, but
	// it names the endpoint the agent sends its API key TO: a compromised or
	// confused orchestrator could set it and exfiltrate a credential the worker
	// provisioned from its own env file (§8), which is exactly the threat the
	// worker-side re-check exists to stop. The reversed dedup order in agentEnv
	// only protects keys the file ALSO sets, so a file with the key and no base
	// URL — a perfectly ordinary config — would have leaked. The endpoint must
	// therefore be paired with the credential in the worker's agent env file.
	//
	// This matches gastown's existing local policy: config/env.go excludes
	// ANTHROPIC_BASE_URL from parent-shell passthrough for the same
	// pairing reason (a MiniMax deacon's base URL leaking into Claude
	// polecats), and alternate-provider presets set base URL and key together
	// in one Env block.
	envAllowedExact = map[string]bool{
		"CLAUDECODE":             true,
		"CLAUDE_CODE_ENTRYPOINT": true,
		"ANTHROPIC_MODEL":        true,
	}
	// envSecretSubstrings refuse anything credential-shaped even when it matches
	// an allowed family (e.g. a hypothetical GT_..._TOKEN): agent credentials
	// come from the worker's own agent env file (§8), never the wire.
	envSecretSubstrings = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "_KEY", "PRIVATE"}

	// envLauncherOnly configure the launcher process itself and must never be
	// forwarded to the agent. (They are credential-shaped too, so EnvSecretKey
	// already refuses them; listed for intent.)
	envLauncherOnly = map[string]bool{"GT_WORKER_TOKEN": true, "GT_WORKER_NAME": true}
)

// EnvAllowed reports whether a session-env key may cross the exec stream.
func EnvAllowed(key string) bool {
	if key == "" || envLauncherOnly[key] || EnvSecretKey(key) {
		return false
	}
	if envAllowedExact[key] {
		return true
	}
	for _, p := range envAllowedPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// EnvSecretKey reports whether a key is credential-shaped and must never ride
// the wire in either direction.
func EnvSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, frag := range envSecretSubstrings {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}

// EnvAllowedDescription renders the policy for an error message.
func EnvAllowedDescription() string {
	return "GT_*, BD_*, ANTHROPIC_DEFAULT_*, and specific model-config keys, none credential-shaped"
}
