package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/runtime"
)

// closeAuditFile is the town-relative log of wisp closes made through
// Beads.Close, Beads.CloseWithReason and Beads.ForceCloseWithReason (both their
// in-process store and bd CLI paths). Wisps live in dolt_ignore'd tables, so
// once a closed wisp is purged neither its close_reason nor who set it survives
// anywhere else; this log is how to tell which code path closed a merge-request
// wisp whose PR is still open (gt-9yhyh).
//
// It is not a log of every wisp close. These paths do not write to it:
//   - bd close run outside those three methods: Beads.CloseSlingContext, the
//     exec.Command("bd", "close", ...) sites in internal/cmd, and any agent or
//     patrol formula that runs bd close from a shell;
//   - reaper and wisp gc purges, which delete rows without closing them;
//   - an isolated Beads (tests).
//
// Covering the remaining paths is tracked in gt-4nf4e.
const closeAuditFile = "logs/wisp-close-audit.log"

type closeAuditEntry struct {
	Time    string   `json:"ts"`
	Actor   string   `json:"actor,omitempty"`
	Role    string   `json:"role,omitempty"`
	Session string   `json:"session,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Reason  string   `json:"reason"`
	IDs     []string `json:"ids"`
	Error   string   `json:"error,omitempty"`
}

// closeAuditInTests lets this package's own tests exercise the audit write,
// which closeAuditAllowed otherwise refuses under go test.
var closeAuditInTests bool

// closeAuditAllowed reports whether this process may write the audit log. A go
// test binary inherits GT_ROOT from the agent session that launched it, so
// without this gate any test that closes a wisp would append fake closes to the
// live town log and corrupt the evidence gt gathers from it.
func closeAuditAllowed() bool {
	return closeAuditInTests || !testing.Testing()
}

// auditWispClose appends one JSON line per close call that touches a wisp.
// Best-effort: an audit failure must never fail or block the close itself.
func (b *Beads) auditWispClose(reason string, ids []string, closeErr error) {
	if b.isolated || !closeAuditAllowed() {
		return
	}
	root := os.Getenv("GT_ROOT")
	if root == "" {
		return
	}
	var wisps []string
	for _, id := range ids {
		if isWispID(id) {
			wisps = append(wisps, id)
		}
	}
	if len(wisps) == 0 {
		return
	}

	entry := closeAuditEntry{
		Time:    time.Now().UTC().Format(time.RFC3339),
		Actor:   os.Getenv("BD_ACTOR"),
		Role:    os.Getenv("GT_ROLE"),
		Session: runtime.SessionIDFromEnv(),
		Reason:  reason,
		IDs:     wisps,
	}
	if cwd, err := os.Getwd(); err == nil {
		entry.Cwd = cwd
	}
	if closeErr != nil {
		entry.Error = closeErr.Error()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}

	path := filepath.Join(root, closeAuditFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
