package workerclient

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// safeToken bounds the identity fields (session/rig/polecat) that flow into
// filesystem paths and the git-clone URL on the worker host. The worker is a
// SEPARATE trust domain from the orchestrator (that is the whole point of the
// TCP-mTLS backend), so authenticating the peer is not enough — a compromised
// or buggy orchestrator must not be able to send "../../etc" and steer
// os.RemoveAll / git clone outside the state dir. Kept deliberately strict.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validIdentityField reports whether v is a safe path/URL component: a
// non-empty [A-Za-z0-9._-]+ token that is neither "." nor "..".
func validIdentityField(v string) bool {
	if v == "." || v == ".." {
		return false
	}
	return safeToken.MatchString(v)
}

// validateSessionFields checks the orchestrator-supplied session/rig/polecat
// wire fields before they reach any filesystem or URL sink.
func validateSessionFields(session, rig, polecat string) error {
	for name, v := range map[string]string{"session": session, "rig": rig, "polecat": polecat} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !validIdentityField(v) {
			return fmt.Errorf("%s %q is not a safe identifier (want %s, not . or ..)", name, v, safeToken.String())
		}
	}
	return nil
}

// safeBranch bounds the orchestrator-supplied branch name before it reaches
// git on the worker host. It admits the polecat branch forms
// (polecat/<name>/<issue>@<ts>, polecat/<name>-<ts>) and nothing git could
// read as an option, a revision expression or a path escape.
var safeBranch = regexp.MustCompile(`^[A-Za-z0-9._@/-]+$`)

// validBranch reports whether v is a branch name safe to hand to git.
func validBranch(v string) bool {
	if !safeBranch.MatchString(v) || strings.HasPrefix(v, "-") {
		return false
	}
	// "HEAD" is what a detached worktree reports for its branch, and git
	// refuses to create a branch of that name.
	if v == "HEAD" {
		return false
	}
	if strings.Contains(v, "..") || strings.Contains(v, "@{") {
		return false
	}
	for _, part := range strings.Split(v, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// Bounds on orchestrator-delivered agent files (session_open files). They are
// small config documents; anything larger is not what this channel is for.
const (
	maxAgentFiles     = 8
	maxAgentFileBytes = 256 << 10
)

// validateAgentFiles bounds the session_open files map.
func validateAgentFiles(files map[string]string) error {
	if len(files) > maxAgentFiles {
		return fmt.Errorf("too many agent files (%d, max %d)", len(files), maxAgentFiles)
	}
	for k, v := range files {
		if k == "" {
			return fmt.Errorf("agent file with an empty path key")
		}
		if len(v) > maxAgentFileBytes {
			return fmt.Errorf("agent file exceeds %d bytes", maxAgentFileBytes)
		}
	}
	return nil
}

// underRoot asserts that a joined path stays within root — defense in depth
// behind validateSessionFields, so a joined path can never escape the state
// dir even if the validation were somehow bypassed.
func underRoot(root, path string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes state dir %q", absPath, absRoot)
	}
	return nil
}
