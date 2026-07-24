package proxy

import (
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// sessionIdentityEnvKeys are the env vars the proxy derives from the
// authenticated client cert and injects into every proxied exec — and
// therefore strips from the base env first, so nothing else (server env,
// future request-supplied env) can ever set them. They are TRUSTED identity:
// spoofing GT_SESSION would let a client heartbeat or act as another session.
var sessionIdentityEnvKeys = []string{"GT_SESSION", "GT_RIG", "GT_POLECAT", "GT_ROLE"}

// prefixCacheTTL bounds how stale the proxy's view of rigs.json can be. Rigs
// are added rarely; a newly added rig's polecats get the default prefix for
// at most this long.
const prefixCacheTTL = 30 * time.Second

// rigPrefix returns the session-name prefix for a rig, reading rigs.json
// through a TTL cache so the long-running proxy tracks newly added rigs.
func (s *Server) rigPrefix(rig string) string {
	s.prefixMu.Lock()
	if s.prefixReg == nil || time.Since(s.prefixAt) > prefixCacheTTL {
		reg, err := session.BuildPrefixRegistryFromTown(s.cfg.TownRoot)
		if err != nil {
			// Keep a previously good registry on transient read errors;
			// otherwise fall through with nil and use the default prefix.
			s.log.Warn("could not build rig prefix registry", "err", err)
			reg = s.prefixReg
		}
		s.prefixReg = reg
		s.prefixAt = time.Now()
	}
	reg := s.prefixReg
	s.prefixMu.Unlock()

	if reg == nil {
		return session.DefaultPrefix
	}
	return reg.PrefixForRig(rig)
}

// sessionIdentityEnv derives the trusted session env for an authenticated
// polecat identity ("<rig>/<name>"). This is what makes the host-side
// heartbeat protocol work for REMOTE polecats (remote-polecat-execution.md
// §8.1): a remote polecat's gt/bd run on the host via this exec path with no
// session env of their own, so without this injection persistentPreRun never
// touches the session heartbeat, gt done never records "exiting", and the
// reaper eventually kills a perfectly healthy remote polecat as stale.
//
// Returns nil for a malformed identity.
func (s *Server) sessionIdentityEnv(identity string) []string {
	idx := strings.LastIndex(identity, "/")
	if idx <= 0 || idx == len(identity)-1 {
		return nil
	}
	rig, name := identity[:idx], identity[idx+1:]
	return []string{
		"GT_SESSION=" + session.PolecatSessionName(s.rigPrefix(rig), name),
		"GT_RIG=" + rig,
		"GT_POLECAT=" + name,
		"GT_ROLE=polecat",
	}
}

// stripEnvKeys removes every entry whose key is in keys.
func stripEnvKeys(env []string, keys []string) []string {
	out := env[:0]
	for _, e := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// prefixCache is embedded in Server: the TTL-cached rigs.json prefix
// registry used to derive session names from authenticated identities.
type prefixCache struct {
	prefixMu  sync.Mutex
	prefixReg *session.PrefixRegistry
	prefixAt  time.Time
}
