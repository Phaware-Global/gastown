package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/runtime"
)

// closeAuditFile is the town-relative log of every wisp close. Wisps live in
// dolt_ignore'd tables, so once a closed wisp is purged neither its close_reason
// nor who set it survives anywhere else; this log is the only way to tell which
// code path closed a merge-request wisp whose PR is still open (gt-9yhyh).
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

// auditWispClose appends one JSON line per close call that touches a wisp.
// Best-effort: an audit failure must never fail or block the close itself.
func (b *Beads) auditWispClose(reason string, ids []string, closeErr error) {
	if b.isolated {
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
