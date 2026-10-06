package cmd

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// setupEscalationTestStore initializes a real bd store on the shared test Dolt
// container. bd init reports its first-run metrics notice as a non-zero exit
// even though the store is created, so that notice alone is not a failure.
func setupEscalationTestStore(t *testing.T) *beads.Beads {
	t.Helper()
	requireBd(t)
	testutil.RequireDoltContainer(t)
	port, _ := strconv.Atoi(testutil.DoltContainerPort())

	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	prefix := "esc" + hex.EncodeToString(buf[:])
	bd := beads.NewIsolatedWithPort(t.TempDir(), port)
	if err := bd.Init(prefix); err != nil && !strings.Contains(err.Error(), "anonymous usage metrics") {
		t.Fatalf("bd init: %v", err)
	}

	dbName := "beads_" + prefix
	t.Cleanup(func() {
		db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%s)/", testutil.DoltContainerPort()))
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_, _ = db.Exec("CALL dolt_purge_dropped_databases()")
	})
	return bd
}

func fingerprintedEscalation(fingerprint string) *beads.EscalationFields {
	return &beads.EscalationFields{
		Severity:    "high",
		Reason:      "fingerprint test",
		EscalatedBy: "test/agent",
		EscalatedAt: "2026-10-06T00:00:00Z",
		Fingerprint: fingerprint,
	}
}

// Runs the real create + lookup path against a real bd store: escalations are
// created as wisps, so the duplicate lookup must see wisps.
func TestCreateEscalationUnlessDuplicate_RealStore(t *testing.T) {
	bd := setupEscalationTestStore(t)

	fpA := escalationFingerprintLabel("test:fingerprint-a")
	fpB := escalationFingerprintLabel("test:fingerprint-b")

	first, existing, err := createEscalationUnlessDuplicate(bd, "first escalation", fingerprintedEscalation(fpA))
	if err != nil {
		t.Fatalf("first escalation: %v", err)
	}
	if existing != nil || first == nil {
		t.Fatalf("first escalation must be created, got created=%v existing=%v", first, existing)
	}

	t.Run("same fingerprint is suppressed and returns the first ID", func(t *testing.T) {
		created, existing, err := createEscalationUnlessDuplicate(bd, "second escalation", fingerprintedEscalation(fpA))
		if err != nil {
			t.Fatalf("second escalation: %v", err)
		}
		if created != nil {
			t.Fatalf("duplicate was created as %s, want suppression", created.ID)
		}
		if existing == nil || existing.ID != first.ID {
			t.Fatalf("suppressed against %v, want first escalation %s", existing, first.ID)
		}
	})

	t.Run("different fingerprint is not suppressed", func(t *testing.T) {
		created, existing, err := createEscalationUnlessDuplicate(bd, "other escalation", fingerprintedEscalation(fpB))
		if err != nil {
			t.Fatalf("other escalation: %v", err)
		}
		if existing != nil || created == nil {
			t.Fatalf("different fingerprint must be created, got created=%v existing=%v", created, existing)
		}
		if created.ID == first.ID {
			t.Fatalf("different fingerprint reused ID %s", first.ID)
		}
	})

	t.Run("no fingerprint is never suppressed", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			created, existing, err := createEscalationUnlessDuplicate(bd, "unfingerprinted", fingerprintedEscalation(""))
			if err != nil {
				t.Fatalf("unfingerprinted escalation %d: %v", i, err)
			}
			if existing != nil || created == nil {
				t.Fatalf("unfingerprinted escalation %d must be created", i)
			}
		}
	})

	t.Run("fingerprint escalates again once the first is closed", func(t *testing.T) {
		if err := bd.CloseEscalation(first.ID, "test/agent", "resolved"); err != nil {
			t.Fatalf("closing first escalation: %v", err)
		}
		created, existing, err := createEscalationUnlessDuplicate(bd, "third escalation", fingerprintedEscalation(fpA))
		if err != nil {
			t.Fatalf("third escalation: %v", err)
		}
		if existing != nil || created == nil {
			t.Fatalf("closed fingerprint must escalate again, got created=%v existing=%v", created, existing)
		}
		if created.ID == first.ID {
			t.Fatalf("new escalation reused closed ID %s", first.ID)
		}
	})
}
