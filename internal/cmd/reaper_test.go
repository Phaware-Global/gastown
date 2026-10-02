package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestReaperDatabaseNamesTrimsConfiguredList(t *testing.T) {
	oldDB := reaperDB
	t.Cleanup(func() { reaperDB = oldDB })

	reaperDB = " hq, gastown ,, beads "
	got := reaperDatabaseNames()
	want := []string{"hq", "gastown", "beads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reaperDatabaseNames() = %#v, want %#v", got, want)
	}
}

func TestWaitBeforeReaperDatabase(t *testing.T) {
	oldDelay := reaperDBDelay
	t.Cleanup(func() { reaperDBDelay = oldDelay })

	reaperDBDelay = "0s"
	if err := waitBeforeReaperDatabase(0); err != nil {
		t.Fatalf("first database wait returned error: %v", err)
	}
	if err := waitBeforeReaperDatabase(1); err != nil {
		t.Fatalf("zero-delay wait returned error: %v", err)
	}

	reaperDBDelay = "not-a-duration"
	if err := waitBeforeReaperDatabase(1); err == nil {
		t.Fatal("invalid delay should return an error")
	}
}

func TestPrintReaperScanTextShowsFastTrackCandidates(t *testing.T) {
	results := []*reaper.ScanResult{
		{Database: "hq", FastTrackCandidates: 30},
		{Database: "gastown", FastTrackCandidates: 2},
	}
	var buf bytes.Buffer
	printReaperScanText(&buf, results)
	out := buf.String()

	if got := strings.Count(out, "  Fast-track:       30\n"); got != 1 {
		t.Errorf("per-DB hq fast-track line count = %d, want 1\n%s", got, out)
	}
	if got := strings.Count(out, "  Fast-track:       2\n"); got != 1 {
		t.Errorf("per-DB gastown fast-track line count = %d, want 1\n%s", got, out)
	}
	if got := strings.Count(out, "  Fast-track:       32\n"); got != 1 {
		t.Errorf("summary fast-track total line count = %d, want 1\n%s", got, out)
	}
}

// TestStaleAgeFlagDefaultMatchesConstant guards against gt-73to recurring:
// the --stale-age flag's default MUST equal reaper.DefaultStaleIssueAge, not
// a hardcoded literal that can drift from it.
func TestStaleAgeFlagDefaultMatchesConstant(t *testing.T) {
	flag := reaperAutoCloseCmd.Flags().Lookup("stale-age")
	if flag == nil {
		t.Fatal("reaperAutoCloseCmd has no --stale-age flag")
	}
	got, err := time.ParseDuration(flag.DefValue)
	if err != nil {
		t.Fatalf("--stale-age default %q is not a parseable duration: %v", flag.DefValue, err)
	}
	if got != reaper.DefaultStaleIssueAge {
		t.Errorf("--stale-age default = %v, want reaper.DefaultStaleIssueAge (%v)", got, reaper.DefaultStaleIssueAge)
	}
}

func TestPrintReaperScanTextOmitsFastTrackWhenZero(t *testing.T) {
	var buf bytes.Buffer
	printReaperScanText(&buf, []*reaper.ScanResult{{Database: "hq"}, {Database: "gastown"}})
	if out := buf.String(); strings.Contains(out, "Fast-track") {
		t.Errorf("zero fast-track candidates should not print a line:\n%s", out)
	}
}

func TestPrintReaperScanTextShowsUnparentedStale(t *testing.T) {
	var buf bytes.Buffer
	printReaperScanText(&buf, []*reaper.ScanResult{{Database: "hq", UnparentedStale: 7}, {Database: "gastown"}})
	out := buf.String()
	if got := strings.Count(out, "  Unparented stale: 7 (never reaped)\n"); got != 2 {
		t.Errorf("Unparented stale line count = %d, want 2 (per-DB and summary)\n%s", got, out)
	}
}

func TestPrintReaperScanTextOmitsUnparentedStaleWhenZero(t *testing.T) {
	var buf bytes.Buffer
	printReaperScanText(&buf, []*reaper.ScanResult{{Database: "hq"}})
	if out := buf.String(); strings.Contains(out, "Unparented stale") {
		t.Errorf("zero unparented_stale should not print a line:\n%s", out)
	}
}
