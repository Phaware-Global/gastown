package execution

// Remote session-env contract (docs/design/remote-polecat-execution.md §7.4,
// §6.1.1). The orchestrator's local `exec env` prefix does not cross the
// boundary — neither provider exec channels nor `docker exec` forward the
// caller's environment — so a remote backend's WrapCommand is responsible for
// landing the session env in the remote agent process, and gt-worker-agent
// points the agent's gt/bd/git at its local relay via the vars below.
//
// Split by sensitivity (§7.4): the session vars passed to WrapCommand and the
// relay-pointer vars below are NON-secret and may travel in the launcher
// payload / env file. Secret env (LLM keys, registry creds, the proxy client
// key) must NEVER appear in the returned argv or launcher payload — it is
// delivered via the provider's worker-side secret mechanism, and the proxy
// key never leaves gt-worker-agent at all.
const (
	// EnvProxyURL points gt/bd (via gt-proxy-client) at the control plane.
	// On a worker this is the LOCAL plaintext relay — http://127.0.0.1:9899
	// for native/host networking, http://host.docker.internal:9899 for
	// bridge-networked containers (§6.1.1) — never the host proxy directly:
	// mTLS terminates in gt-worker-agent, and the agent holds no cert.
	EnvProxyURL = "GT_PROXY_URL"

	// EnvProxyCert / EnvProxyKey / EnvProxyCA configure gt-proxy-client for
	// DIRECT mTLS to the host proxy. They are set only where the relay model
	// is not in play (host-local tooling, tests); on workers they stay unset
	// — the private key must never enter the work container or its env.
	EnvProxyCert = "GT_PROXY_CERT"
	EnvProxyKey  = "GT_PROXY_KEY"
	EnvProxyCA   = "GT_PROXY_CA"
)
