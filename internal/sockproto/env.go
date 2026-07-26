package sockproto

import (
	"regexp"
	"strings"
)

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
	// vars, beads, and the model-config carry-over. The wholesale families are
	// narrowed by envEndpointSuffixes below.
	envAllowedPrefixes = []string{"GT_", "BD_", "ANTHROPIC_DEFAULT_"}

	// envEndpointSuffixes refuse anything that NAMES A DESTINATION, whatever
	// family it matched. An endpoint is a worker-LOCAL fact — the agent's
	// control-plane URL is the worker's own session relay, chosen by the worker
	// — so an orchestrator-supplied one is at best wrong and at worst a
	// redirect: GT_PROXY_URL would point the agent's gt/bd RPC at an attacker
	// (argument exfiltration, and injected responses become fake mail/beads,
	// i.e. prompt injection), GT_OTEL_LOGS_URL would ship agent output to a
	// chosen collector, GT_DOLT_HOST pairs with a file-provisioned
	// GT_DOLT_PASSWORD the same way ANTHROPIC_BASE_URL paired with the API key.
	// Refusing by SHAPE rather than by name means a var added later cannot
	// quietly reopen the class.
	envEndpointSuffixes = []string{"_URL", "_URI", "_HOST", "_ADDR", "_ADDRESS", "_ENDPOINT", "_PORT", "_SERVER", "_PROXY"}
	// envAllowedExact are individually permitted keys. They are model/mode
	// SELECTION only — nothing here names a destination.
	//
	// ANTHROPIC_BASE_URL is deliberately absent (and now also caught by the
	// _URL shape rule). It is not itself a secret, but
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

	// envNeverFromWire are keys the orchestrator must never supply, whatever
	// their shape.
	//
	// GT_WORKER_TOKEN / GT_WORKER_NAME configure the LAUNCHER process itself
	// (they are credential-shaped too, so EnvSecretKey already refuses them;
	// listed for intent).
	//
	// GT_PROXY_RELAY is different and the reason this list is not just about the
	// launcher: it tells gt-proxy-client that a non-loopback proxy URL is a
	// worker-local relay, which SUPPRESSES that binary's loopback guard. Only
	// the worker is in a position to assert that. It is inert from the wire
	// today — the paired GT_PROXY_URL is refused as an endpoint and the worker
	// sets it from its own relay — but a security toggle whose safety depends on
	// a second refusal elsewhere is one refactor away from being a redirect.
	envNeverFromWire = map[string]bool{
		"GT_WORKER_TOKEN": true,
		"GT_WORKER_NAME":  true,
		"GT_PROXY_RELAY":  true,
	}
)

// EnvAllowed reports whether a session-env key may cross the exec stream.
func EnvAllowed(key string) bool {
	if key == "" || envNeverFromWire[key] || EnvSecretKey(key) || EnvEndpointKey(key) {
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

// EnvEndpointKey reports whether a key names a destination. Endpoints are
// worker-local facts and are never accepted from the orchestrator; the worker
// supplies the agent's control-plane URL from its own session relay.
func EnvEndpointKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, suffix := range envEndpointSuffixes {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// platformTag matches "<goos>-<goarch>". Platform strings cross the wire in
// BOTH directions — the worker reports its own and its container's in
// hello_ack, the orchestrator tags pushes with one — and each side joins the
// other's value to a filesystem path. So the shape check lives here, enforced
// identically by sender and receiver; validating on one side only is how a
// traversal gets in.
var platformTag = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)

// ValidPlatformTag reports whether s is a well-formed "<goos>-<goarch>" tag.
func ValidPlatformTag(s string) bool { return platformTag.MatchString(s) }

// PlatformTag renders a platform the one way both sides read it.
func PlatformTag(goos, goarch string) string { return goos + "-" + goarch }

// EnvAllowedDescription renders the policy for an error message.
func EnvAllowedDescription() string {
	return "GT_*, BD_*, ANTHROPIC_DEFAULT_*, and specific model-selection keys — none credential-shaped, none naming an endpoint (the worker supplies those itself)"
}
