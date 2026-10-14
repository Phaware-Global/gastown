package beads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditWispCloseRecordsOnlyWisps(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GT_ROOT", root)
	t.Setenv("BD_ACTOR", "gastown/polecats/goose")
	b := &Beads{workDir: root}

	b.auditWispClose("superseded by gt-wisp-new", []string{"gt-wisp-abc", "gt-123"}, nil)
	b.auditWispClose("plain", []string{"gt-456"}, nil)
	b.auditWispClose("boom", []string{"hi-wisp-p5w"}, errors.New("bd failed"))

	data, err := os.ReadFile(filepath.Join(root, closeAuditFile))
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d, want 2 (non-wisp close must not be logged): %s", len(lines), data)
	}
	if !strings.Contains(lines[0], `"ids":["gt-wisp-abc"]`) ||
		!strings.Contains(lines[0], `"reason":"superseded by gt-wisp-new"`) ||
		!strings.Contains(lines[0], `"actor":"gastown/polecats/goose"`) {
		t.Errorf("first entry missing id/reason/actor: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"error":"bd failed"`) {
		t.Errorf("failed close must record the error: %s", lines[1])
	}
}

func TestAuditWispCloseSkippedWhenIsolated(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GT_ROOT", root)
	b := &Beads{workDir: root, isolated: true}

	b.auditWispClose("x", []string{"gt-wisp-abc"}, nil)

	if _, err := os.Stat(filepath.Join(root, closeAuditFile)); !os.IsNotExist(err) {
		t.Fatalf("isolated Beads must not write the audit log, stat err = %v", err)
	}
}
