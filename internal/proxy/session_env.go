package proxy

import (
	"os"
	"path/filepath"
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
//
// This sits on the exec hot path, so the file I/O of a refresh happens
// OUTSIDE the lock (two goroutines racing a refresh just build twice — the
// build is a read-only file parse), and reads take only an RLock. The proxy
// never calls BuildPrefixRegistryFromTown, whose copyFileIfNewer side effect
// would make a daemon request path write into town state.
func (s *Server) rigPrefix(rig string) string {
	s.prefixMu.RLock()
	reg, at := s.prefixReg, s.prefixAt
	s.prefixMu.RUnlock()

	if reg == nil || time.Since(at) > prefixCacheTTL {
		fresh, err := s.buildPrefixRegistry()
		if err != nil {
			// Keep a previously good registry on transient read errors.
			s.log.Warn("could not build rig prefix registry", "err", err)
			fresh = reg
		}
		s.prefixMu.Lock()
		// Another goroutine may have refreshed while we built; last write
		// wins — both saw current file contents.
		s.prefixReg = fresh
		s.prefixAt = time.Now()
		reg = fresh
		s.prefixMu.Unlock()
	}

	if reg == nil {
		return session.DefaultPrefix
	}
	return reg.PrefixForRig(rig)
}

// buildPrefixRegistry reads rigs.json read-only: canonical mayor/rigs.json
// first, town-root fallback second — the same preference as
// session.BuildPrefixRegistryFromTown, minus its fallback-copy write.
func (s *Server) buildPrefixRegistry() (*session.PrefixRegistry, error) {
	canonical := filepath.Join(s.cfg.TownRoot, "mayor", "rigs.json")
	if _, err := os.Stat(canonical); err == nil {
		return session.BuildPrefixRegistryFromFile(canonical)
	}
	return session.BuildPrefixRegistryFromFile(filepath.Join(s.cfg.TownRoot, "rigs.json"))
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
	prefixMu  sync.RWMutex
	prefixReg *session.PrefixRegistry
	prefixAt  time.Time
}
