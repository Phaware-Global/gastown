package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/reaper"
)

func TestGetTTL(t *testing.T) {
	ttls := defaultTTLs

	tests := []struct {
		wispType string
		want     time.Duration
	}{
		{"heartbeat", 6 * time.Hour},
		{"ping", 6 * time.Hour},
		{"patrol", 24 * time.Hour},
		{"gc_report", 24 * time.Hour},
		{"error", 7 * 24 * time.Hour},
		{"recovery", 7 * 24 * time.Hour},
		{"escalation", 7 * 24 * time.Hour},
		{"default", 24 * time.Hour},
		{"", 24 * time.Hour},        // empty falls back to default
		{"unknown", 24 * time.Hour}, // unknown falls back to default
	}

	for _, tc := range tests {
		t.Run(tc.wispType, func(t *testing.T) {
			got := getTTL(ttls, tc.wispType)
			if got != tc.want {
				t.Errorf("getTTL(%q) = %v, want %v", tc.wispType, got, tc.want)
			}
		})
	}
}

func TestWispAge(t *testing.T) {
	now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		updatedAt string
		wantAge   time.Duration
		wantErr   bool
	}{
		{
			name:      "RFC3339",
			updatedAt: "2026-02-07T06:00:00Z",
			wantAge:   6 * time.Hour,
		},
		{
			name:      "one day old",
			updatedAt: "2026-02-06T12:00:00Z",
			wantAge:   24 * time.Hour,
		},
		{
			name:      "invalid",
			updatedAt: "not-a-date",
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &compactIssue{
				Issue: beads.Issue{UpdatedAt: tc.updatedAt},
			}
			got, err := wispAge(w, now)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantAge {
				t.Errorf("wispAge = %v, want %v", got, tc.wantAge)
			}
		})
	}
}

func TestHasKeepLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"no labels", nil, false},
		{"other labels", []string{"bug", "urgent"}, false},
		{"keep label", []string{"keep"}, true},
		{"gt:keep label", []string{"bug", "gt:keep"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &compactIssue{
				Issue: beads.Issue{Labels: tc.labels},
			}
			if got := hasKeepLabel(w); got != tc.want {
				t.Errorf("hasKeepLabel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasComments(t *testing.T) {
	tests := []struct {
		name  string
		count int
		want  bool
	}{
		{"no comments", 0, false},
		{"has comments", 3, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &compactIssue{CommentCount: tc.count}
			if got := hasComments(w); got != tc.want {
				t.Errorf("hasComments = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsReferenced(t *testing.T) {
	tests := []struct {
		name    string
		depCnt  int
		deptCnt int
		want    bool
	}{
		{"no refs", 0, 0, false},
		{"has dependents", 0, 1, true},
		{"has dependencies", 1, 0, true},
		{"both", 2, 3, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &compactIssue{
				Issue: beads.Issue{
					DependencyCount: tc.depCnt,
					DependentCount:  tc.deptCnt,
				},
			}
			if got := isReferenced(w); got != tc.want {
				t.Errorf("isReferenced = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompactTruncate(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"short ASCII", "short", 10, "short"},
		{"exact length", "exactly10!", 10, "exactly10!"},
		{"ASCII too long", "this is too long", 10, "this is..."},
		{"short maxLen", "ab", 3, "ab"},
		{"maxLen 3", "abcdef", 3, "abc"},
		// Multi-byte UTF-8: emoji is 1 rune, not 4 bytes
		{"emoji within limit", "🤝 HANDOFF", 10, "🤝 HANDOFF"},
		{"emoji truncated", "🤝 HANDOFF: Routine cycle for witness", 15, "🤝 HANDOFF: R..."},
		// CJK characters: each is 1 rune, 3 bytes
		{"CJK within limit", "日本語テスト", 10, "日本語テスト"},
		{"CJK truncated", "日本語テストデータ", 6, "日本語..."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactTruncate(tc.s, tc.maxLen); got != tc.want {
				t.Errorf("compactTruncate(%q, %d) = %q, want %q", tc.s, tc.maxLen, got, tc.want)
			}
		})
	}
}

func TestExtractJSONArray(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			"clean JSON array",
			`[{"id":"test"}]`,
			`[{"id":"test"}]`,
		},
		{
			"warning prefix before JSON",
			"Warning: no route found for prefix \"gt-\"\n[{\"id\":\"test\"}]",
			`[{"id":"test"}]`,
		},
		{
			"unicode warning prefix",
			"⚠ Warning: something with 🤝 emoji\n[{\"id\":\"test\"}]",
			`[{"id":"test"}]`,
		},
		{
			"no array in data",
			"just some text without json",
			"just some text without json",
		},
		{
			"empty data",
			"",
			"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(extractJSONArray([]byte(tc.data)))
			if got != tc.want {
				t.Errorf("extractJSONArray(%q) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestLoadTTLConfigDefaults(t *testing.T) {
	// With empty town root, should return defaults
	ttls := loadTTLConfig("", "")

	if ttls["heartbeat"] != 6*time.Hour {
		t.Errorf("heartbeat TTL = %v, want 6h", ttls["heartbeat"])
	}
	if ttls["patrol"] != 24*time.Hour {
		t.Errorf("patrol TTL = %v, want 24h", ttls["patrol"])
	}
	if ttls["error"] != 7*24*time.Hour {
		t.Errorf("error TTL = %v, want 168h", ttls["error"])
	}
}

func TestLoadTTLConfigWithRoleDefaults(t *testing.T) {
	// With empty town root, should return hardcoded defaults
	ttls := loadTTLConfigWithRole("", "")

	for k, want := range defaultTTLs {
		if got := ttls[k]; got != want {
			t.Errorf("loadTTLConfigWithRole TTLs[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestLoadTTLConfigWithRoleSkipsInvalidPaths(t *testing.T) {
	// With nonexistent paths, rig bead lookup should gracefully skip
	ttls := loadTTLConfigWithRole("/nonexistent/town", "myrig")

	// Should still have defaults even though lookups failed
	if ttls["patrol"] != defaultTTLs["patrol"] {
		t.Errorf("patrol TTL = %v, want %v", ttls["patrol"], defaultTTLs["patrol"])
	}
	if ttls["error"] != defaultTTLs["error"] {
		t.Errorf("error TTL = %v, want %v", ttls["error"], defaultTTLs["error"])
	}
}

func TestCleanOrphanedWispDepsUsesTypedTargets(t *testing.T) {
	data, err := os.ReadFile("compact.go")
	if err != nil {
		t.Fatalf("read compact.go: %v", err)
	}
	body := compactSourceBetween(t, string(data), "func cleanOrphanedWispDeps(", "// listWisps")
	if strings.Contains(body, "depends_on_id") {
		t.Fatalf("cleanOrphanedWispDeps should not use legacy depends_on_id:\n%s", body)
	}
	for _, want := range []string{
		"depends_on_wisp_id IS NOT NULL AND NOT EXISTS",
		"wisps WHERE id = wisp_dependencies.depends_on_wisp_id",
		"depends_on_issue_id IS NOT NULL AND NOT EXISTS",
		"issues WHERE id = wisp_dependencies.depends_on_issue_id",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("cleanOrphanedWispDeps missing %q:\n%s", want, body)
		}
	}
}

func compactSourceBetween(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start == -1 {
		t.Fatalf("could not find %q", startMarker)
	}
	end := strings.Index(source[start:], endMarker)
	if end == -1 {
		t.Fatalf("could not find %q after %q", endMarker, startMarker)
	}
	return source[start : start+end]
}

// fakeBD stands in for the bd CLI. It answers the scan's `bd sql` queries
// from canned rows and records every other call, so tests can assert exactly
// which writes a run issued.
type fakeBD struct {
	wisps    string   // rows for the main scan
	labels   string   // rows for wisp_labels
	comments string   // rows for wisp_comments
	count    string   // rows for the blind-scan count
	tables   []string // SHOW TABLES; nil means the full wisp schema
	columns  string   // SHOW COLUMNS FROM wisp_dependencies; "" means the full set

	// failWrite, if set, is consulted for every write; a non-nil error fails it.
	failWrite func(args []string) error

	queries []string   // every `bd sql --json` query
	writes  [][]string // every call that is not a bd sql read
}

func newFakeBD(wisps string) *fakeBD {
	return &fakeBD{wisps: wisps, labels: "[]", comments: "[]", count: `[{"n":0}]`}
}

func (f *fakeBD) run(args ...string) ([]byte, error) {
	if len(args) >= 3 && args[0] == "sql" && args[1] == "--csv" {
		switch {
		case args[2] == "SHOW TABLES":
			tables := f.tables
			if tables == nil {
				tables = requiredWispTables
			}
			return []byte("Tables_in_test\n" + strings.Join(tables, "\n") + "\n"), nil
		case strings.HasPrefix(args[2], "SHOW COLUMNS"):
			if f.columns != "" {
				return []byte(f.columns), nil
			}
			return []byte("Field,Type\nissue_id,varchar\ndepends_on_wisp_id,varchar\ndepends_on_issue_id,varchar\ndepends_on_external,varchar\n"), nil
		}
	}
	if len(args) >= 3 && args[0] == "sql" && args[1] == "--json" {
		q := args[2]
		f.queries = append(f.queries, q)
		switch {
		case strings.HasPrefix(q, "SELECT COUNT(*) AS n"):
			return []byte(f.count), nil
		case strings.HasPrefix(q, "SELECT w.id AS id"):
			return []byte(f.wisps), nil
		case strings.HasPrefix(q, "SELECT issue_id, label FROM wisp_labels"):
			return []byte(f.labels), nil
		case strings.HasPrefix(q, "SELECT issue_id, COUNT(*)"):
			return []byte(f.comments), nil
		}
		return nil, os.ErrInvalid
	}
	if len(args) >= 2 && args[0] == "sql" {
		return []byte("OK, 0 rows affected"), nil
	}
	f.writes = append(f.writes, args)
	if f.failWrite != nil {
		if err := f.failWrite(args); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// writesOf returns the recorded writes whose bd subcommand is verb.
func (f *fakeBD) writesOf(verb string) [][]string {
	var out [][]string
	for _, w := range f.writes {
		if w[0] == verb {
			out = append(out, w)
		}
	}
	return out
}

func wispRow(id, status, updated string, extra string) string {
	return `{"id":"` + id + `","title":"t ` + id + `","status":"` + status + `","issue_type":"task","wisp_type":"",` +
		`"created_at":"` + updated + `","updated_at":"` + updated + `"` + extra + `}`
}

// manyWispRows returns n wisps, ids <prefix>0000.. in ascending age order
// (oldest first), all long past any default TTL.
func manyWispRows(prefix, status string, n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = wispRow(fmt.Sprintf("%s%04d", prefix, i), status, "2026-01-01T00:00:00Z", "")
	}
	return "[" + strings.Join(rows, ",") + "]"
}

var compactTestNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// withCompactFlags isolates the package-level flags and zeroes the write pause.
func withCompactFlags(t *testing.T) {
	t.Helper()
	oldDry, oldJSON, oldVerbose, oldPause := compactDryRun, compactJSON, compactVerbose, compactBatchPause
	compactDryRun, compactJSON, compactVerbose, compactBatchPause = false, true, false, 0
	t.Cleanup(func() {
		compactDryRun, compactJSON, compactVerbose, compactBatchPause = oldDry, oldJSON, oldVerbose, oldPause
	})
}

var testCompactLimits = compactLimits{MaxPromotes: defaultCompactMaxPromotes, MaxDeletes: defaultCompactMaxDeletes}

func TestListWispsReadsWispsTable(t *testing.T) {
	wisps := "[" + strings.Join([]string{
		wispRow("gt-wisp-a", "open", "2026-10-01T00:00:00Z", `,"has_parent":0,"parent_open":0,"is_mr":0,"mr_purgeable":1`),
		wispRow("gt-wisp-b", "open", "2026-10-01T00:00:00Z", `,"has_parent":1,"parent_open":1,"is_mr":0,"mr_purgeable":1`),
		wispRow("gt-wisp-c", "closed", "2026-10-01T00:00:00Z", `,"has_parent":0,"parent_open":0,"is_mr":1,"mr_purgeable":0`),
	}, ",") + "]"
	f := newFakeBD(wisps)
	f.labels = `[{"issue_id":"gt-wisp-a","label":"keep"}]`
	f.comments = `[{"issue_id":"gt-wisp-a","n":2}]`

	got, err := listWispsWith(f.run)
	if err != nil {
		t.Fatalf("listWispsWith: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("scanned %d wisps, want 3", len(got))
	}
	a, b, c := got[0], got[1], got[2]
	if a.ID != "gt-wisp-a" || a.CommentCount != 2 || !hasKeepLabel(a) || !a.Ephemeral || a.HasParent || a.IsMR || !a.MRPurgeable {
		t.Errorf("wisp a mis-decoded: %+v", a)
	}
	if !b.HasParent || !b.ParentOpen {
		t.Errorf("wisp b parentage mis-decoded: %+v", b)
	}
	if !c.IsMR || c.MRPurgeable {
		t.Errorf("wisp c merge-request state mis-decoded: %+v", c)
	}
}

// The scan must reuse the reaper's eligibility SQL, not a copy: agent identity
// exclusion (both label tables) and the merge-request recovery window.
func TestListWispsUsesReaperEligibilitySQL(t *testing.T) {
	f := newFakeBD("[]")
	if _, err := listWispsWith(f.run); err != nil {
		t.Fatalf("listWispsWith: %v", err)
	}
	var scan string
	for _, q := range f.queries {
		if strings.HasPrefix(q, "SELECT w.id AS id") {
			scan = q
		}
	}
	if scan == "" {
		t.Fatalf("no scan query issued: %v", f.queries)
	}
	agentJoin, agentWhere := reaper.NotAgentWispJoin("w")
	mrJoin, mrWhere := reaper.PurgeableMRWispJoin("w")
	parentJoin, _ := reaper.ParentExcludeJoin("")
	for _, want := range []string{agentJoin, agentWhere, mrJoin, mrWhere, parentJoin} {
		if !strings.Contains(scan, want) {
			t.Errorf("scan query is missing reaper SQL %q:\n%s", want, scan)
		}
	}
	if !strings.Contains(mrWhere, "INTERVAL 24 HOUR") {
		t.Errorf("reaper MR window changed from 24h: %s", mrWhere)
	}
}

// TestListWispsDoesNotDependOnBdList pins the gt-jux2 regression: bd list
// returns no wisps, so a scan built on it saw zero rows forever.
func TestListWispsDoesNotDependOnBdList(t *testing.T) {
	f := newFakeBD("[" + wispRow("gt-wisp-a", "open", "2026-10-01T00:00:00Z", "") + "]")
	guarded := func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			t.Errorf("listWisps must not call bd list: %v", args)
		}
		return f.run(args...)
	}
	got, err := listWispsWith(guarded)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d wisps, err %v; want 1", len(got), err)
	}
}

func TestListWispsPropagatesQueryErrors(t *testing.T) {
	_, err := listWispsWith(func(args ...string) ([]byte, error) { return nil, os.ErrPermission })
	if err == nil {
		t.Fatal("expected error when the wisps query fails, got silent empty result")
	}
	if errors.Is(err, errNoWispTables) {
		t.Fatal("a failing bd must not be mistaken for a database with no wisp tables")
	}
}

// --- schema gate (reaper's HasReaperSchema parity) ---

func TestListWispsReportsMissingSchema(t *testing.T) {
	for name, f := range map[string]*fakeBD{
		"no wisps table": func() *fakeBD {
			f := newFakeBD("[]")
			f.tables = []string{"issues", "labels"}
			return f
		}(),
		"no labels table": func() *fakeBD {
			f := newFakeBD("[]")
			f.tables = []string{"wisps", "wisp_labels", "wisp_comments", "wisp_dependencies", "issues"}
			return f
		}(),
		"old wisp_dependencies columns": func() *fakeBD {
			f := newFakeBD("[]")
			f.columns = "Field,Type\nissue_id,varchar\ndepends_on_id,varchar\n"
			return f
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := listWispsWith(f.run)
			if !errors.Is(err, errNoWispTables) {
				t.Fatalf("err = %v, want errNoWispTables", err)
			}
		})
	}
}

func TestRunCompactOnDatabaseWithoutWispTablesSucceeds(t *testing.T) {
	withCompactFlags(t)
	f := newFakeBD("[]")
	f.tables = []string{"issues"}

	result, err := runCompactWith(f.run, defaultTTLs, compactTestNow, testCompactLimits)
	if err != nil {
		t.Fatalf("a database without wisp tables must not fail compaction: %v", err)
	}
	if result == nil || len(result.Deleted)+len(result.Promoted) != 0 {
		t.Fatalf("result = %+v, want empty", result)
	}
	if len(f.writes) != 0 {
		t.Fatalf("issued writes against a database with no wisp tables: %v", f.writes)
	}
}

// --- per-run cap and batching ---

func TestPlanCompactionCapsDeletesOldestFirst(t *testing.T) {
	f := newFakeBD(manyWispRows("d", "closed", 1200))
	wisps, err := listWispsWith(f.run)
	if err != nil {
		t.Fatal(err)
	}
	plan := planWispCompaction(wisps, defaultTTLs, compactTestNow, compactLimits{MaxPromotes: 100, MaxDeletes: 500})
	if len(plan.Delete) != 500 {
		t.Fatalf("planned %d deletes, want the cap of 500", len(plan.Delete))
	}
	if plan.DeferredDeletes != 700 {
		t.Errorf("deferred deletes = %d, want 700", plan.DeferredDeletes)
	}
	if plan.Delete[0].ID != "d0000" || plan.Delete[499].ID != "d0499" {
		t.Errorf("cap must keep the oldest-first order: first %s last %s", plan.Delete[0].ID, plan.Delete[499].ID)
	}
}

func TestPlanCompactionCapsPromotes(t *testing.T) {
	f := newFakeBD(manyWispRows("p", "open", 300))
	wisps, _ := listWispsWith(f.run)
	plan := planWispCompaction(wisps, defaultTTLs, compactTestNow, compactLimits{MaxPromotes: 100, MaxDeletes: 500})
	if len(plan.Promote) != 100 || plan.DeferredPromotes != 200 {
		t.Fatalf("promotes %d deferred %d, want 100 and 200", len(plan.Promote), plan.DeferredPromotes)
	}
}

func TestRunCompactBatchesDeletes(t *testing.T) {
	withCompactFlags(t)
	f := newFakeBD(manyWispRows("d", "closed", 1200))

	result, err := runCompactWith(f.run, defaultTTLs, compactTestNow, testCompactLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 500 || result.DeferredDeletes != 700 {
		t.Fatalf("deleted %d deferred %d, want 500 and 700", len(result.Deleted), result.DeferredDeletes)
	}
	calls := f.writesOf("delete")
	if len(calls) != 10 {
		t.Fatalf("%d bd delete calls for 500 wisps, want 10 batched calls of 50", len(calls))
	}
	for _, c := range calls {
		// delete <ids...> --force
		if ids := len(c) - 2; ids != 50 || c[len(c)-1] != "--force" {
			t.Fatalf("bad batch %v", c[:3])
		}
	}
}

func TestRunCompactBatchFailureFallsBackToSingles(t *testing.T) {
	withCompactFlags(t)
	f := newFakeBD(manyWispRows("d", "closed", 3))
	f.failWrite = func(args []string) error {
		if len(args) > 3 { // the batch call; single-id calls succeed
			return os.ErrInvalid
		}
		return nil
	}
	result, err := runCompactWith(f.run, defaultTTLs, compactTestNow, testCompactLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 3 {
		t.Fatalf("deleted %d, want 3 via single-id fallback (errors: %v)", len(result.Deleted), result.Errors)
	}
}

func TestRunCompactAbortsAfterRepeatedWriteFailures(t *testing.T) {
	withCompactFlags(t)
	f := newFakeBD(manyWispRows("d", "closed", 200))
	f.failWrite = func([]string) error { return os.ErrInvalid }

	result, err := runCompactWith(f.run, defaultTTLs, compactTestNow, testCompactLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("deleted %d with every write failing", len(result.Deleted))
	}
	if got := len(f.writes); got > 1+compactMaxConsecutiveFailures {
		t.Fatalf("issued %d writes against a failing server, want at most %d", got, 1+compactMaxConsecutiveFailures)
	}
	if !strings.Contains(strings.Join(result.Errors, "\n"), "aborting") {
		t.Errorf("abort not reported: %v", result.Errors)
	}
	if result.DeferredDeletes == 0 {
		t.Errorf("remaining work should be reported as deferred")
	}
}

func TestRunCompactDryRunWritesNothing(t *testing.T) {
	withCompactFlags(t)
	compactDryRun = true
	f := newFakeBD(manyWispRows("d", "closed", 10))

	result, err := runCompactWith(f.run, defaultTTLs, compactTestNow, testCompactLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("dry run issued writes: %v", f.writes)
	}
	if len(result.Deleted) != 10 {
		t.Fatalf("dry run should report the 10 planned deletes, got %d", len(result.Deleted))
	}
}

func TestRunCompactRejectsNonPositiveCaps(t *testing.T) {
	oldP, oldD := compactMaxPromotes, compactMaxDeletes
	t.Cleanup(func() { compactMaxPromotes, compactMaxDeletes = oldP, oldD })
	for _, c := range [][2]int{{0, 500}, {100, 0}, {-1, -1}} {
		compactMaxPromotes, compactMaxDeletes = c[0], c[1]
		if err := runCompact(nil, nil); err == nil {
			t.Errorf("caps %v accepted, want an error", c)
		}
	}
}

// --- exemptions: merge-request wisps, live assignments, mail ---

func TestPlanCompactionNeverTouchesOpenMergeRequestWisp(t *testing.T) {
	for _, status := range []string{"open", "in_progress", "hooked"} {
		t.Run(status, func(t *testing.T) {
			f := newFakeBD("[" + wispRow("mr", status, "2026-01-01T00:00:00Z", `,"is_mr":1,"mr_purgeable":1`) + "]")
			wisps, _ := listWispsWith(f.run)
			plan := planWispCompaction(wisps, defaultTTLs, compactTestNow, testCompactLimits)
			if len(plan.Delete)+len(plan.Promote) != 0 {
				t.Fatalf("open MR wisp was planned for a write: %+v", plan)
			}
		})
	}
}

func TestPlanCompactionKeepsClosedMergeRequestInsideRecoveryWindow(t *testing.T) {
	// Closed unmerged <24h ago: the reaper's MR window says not purgeable, even
	// though updated_at and a lowered TTL make it look expired.
	f := newFakeBD("[" + wispRow("mr", "closed", "2026-10-17T11:00:00Z", `,"is_mr":1,"mr_purgeable":0`) + "]")
	wisps, _ := listWispsWith(f.run)
	ttls := map[string]time.Duration{"default": time.Minute}
	plan := planWispCompaction(wisps, ttls, compactTestNow, testCompactLimits)
	if len(plan.Delete) != 0 {
		t.Fatalf("closed MR wisp inside the recovery window was planned for delete")
	}
}

func TestPlanCompactionDeletesPurgeableClosedMergeRequest(t *testing.T) {
	f := newFakeBD("[" + wispRow("mr", "closed", "2026-01-01T00:00:00Z", `,"is_mr":1,"mr_purgeable":1`) + "]")
	wisps, _ := listWispsWith(f.run)
	plan := planWispCompaction(wisps, defaultTTLs, compactTestNow, testCompactLimits)
	if len(plan.Delete) != 1 {
		t.Fatalf("merged/old closed MR wisp should still age out, got %+v", plan)
	}
}

func TestPlanCompactionLeavesLiveAssignmentsAndOpenMail(t *testing.T) {
	wisps := "[" + strings.Join([]string{
		wispRow("hooked", "hooked", "2026-01-01T00:00:00Z", ""),
		wispRow("pinned", "pinned", "2026-01-01T00:00:00Z", ""),
		wispRow("mail", "open", "2026-01-01T00:00:00Z", ""),
		wispRow("plain", "open", "2026-01-01T00:00:00Z", ""),
	}, ",") + "]"
	f := newFakeBD(wisps)
	f.labels = `[{"issue_id":"mail","label":"gt:message"}]`
	got, _ := listWispsWith(f.run)
	plan := planWispCompaction(got, defaultTTLs, compactTestNow, testCompactLimits)
	if len(plan.Promote) != 1 || plan.Promote[0].ID != "plain" {
		t.Fatalf("only the plain stuck wisp should be promoted, got %+v", plan.Promote)
	}
	if plan.Skipped != 3 {
		t.Errorf("skipped = %d, want 3 exempt", plan.Skipped)
	}
}

// --- blind-scan warning counts the scan's own population ---

func TestBlindScanWarningUsesAgentFilter(t *testing.T) {
	f := newFakeBD("[]")
	f.count = `[{"n":0}]` // the filtered population is empty: only agent beads exist
	if w := blindScanWarning(f.run); w != "" {
		t.Fatalf("warned on a table of only agent beads: %q", w)
	}
	var q string
	for _, got := range f.queries {
		if strings.HasPrefix(got, "SELECT COUNT(*)") {
			q = got
		}
	}
	agentJoin, agentWhere := reaper.NotAgentWispJoin("w")
	if !strings.Contains(q, agentJoin) || !strings.Contains(q, agentWhere) {
		t.Fatalf("blind-scan count does not apply the scan's agent exclusion:\n%s", q)
	}

	f.count = `[{"n":7}]`
	if w := blindScanWarning(f.run); !strings.Contains(w, "7") {
		t.Fatalf("expected a warning naming 7 rows, got %q", w)
	}
}

func TestDetectAnomaliesBlindScan(t *testing.T) {
	empty := buildReport("2026-10-17", &compactResult{}, nil)
	got := detectAnomalies(empty)
	if len(got) != 1 || !strings.Contains(got[0], "compactor may be blind") {
		t.Errorf("empty scan anomalies = %v, want single blind-scan warning", got)
	}
	for _, a := range got {
		if strings.Contains(a, "agents may be down") {
			t.Errorf("empty scan must not infer patrol agents are down: %q", a)
		}
	}

	// A working scan that really sees no patrols may still flag them.
	hb := []*compactIssue{{WispType: "heartbeat"}}
	got = detectAnomalies(buildReport("2026-10-17", &compactResult{}, hb))
	found := false
	for _, a := range got {
		found = found || strings.Contains(a, "0 patrol wisps")
	}
	if !found {
		t.Errorf("scan with wisps but no patrols should still flag patrols: %v", got)
	}
}

func TestApplyTTLLabelsRejectsNonPositive(t *testing.T) {
	for _, value := range []string{"0s", "0", "-1h", "-30m"} {
		t.Run(value, func(t *testing.T) {
			ttls := map[string]time.Duration{"default": 24 * time.Hour, "patrol": 24 * time.Hour}
			applyTTLLabels(ttls, []string{"wisp_ttl_default:" + value, "wisp_ttl_patrol:" + value})
			if ttls["default"] != 24*time.Hour || ttls["patrol"] != 24*time.Hour {
				t.Errorf("non-positive TTL %q must be ignored, got default=%v patrol=%v", value, ttls["default"], ttls["patrol"])
			}
		})
	}
}

func TestApplyTTLConfigRejectsNonPositive(t *testing.T) {
	ttls := map[string]time.Duration{"default": 24 * time.Hour}
	applyTTLConfig(ttls, map[string]interface{}{"default": "0s", "patrol": "-2h"})
	if ttls["default"] != 24*time.Hour {
		t.Errorf("default TTL = %v, want unchanged 24h", ttls["default"])
	}
	if _, ok := ttls["patrol"]; ok {
		t.Errorf("negative patrol TTL must not be stored, got %v", ttls["patrol"])
	}
}

func TestApplyTTLOverridesAcceptPositive(t *testing.T) {
	ttls := map[string]time.Duration{"default": 24 * time.Hour}
	applyTTLLabels(ttls, []string{"wisp_ttl_default:48h", "unrelated:label"})
	applyTTLConfig(ttls, map[string]interface{}{"patrol": "2h"})
	if ttls["default"] != 48*time.Hour || ttls["patrol"] != 2*time.Hour {
		t.Errorf("positive overrides not applied: %v", ttls)
	}
}

// Wisps are dolt-ignored, so a deleted wisp has no AS OF history to read back.
func TestCompactDocsDoNotPromiseDeletedWispsAreRecoverable(t *testing.T) {
	if strings.Contains(compactCmd.Long, "AS OF") {
		t.Errorf("compact help claims Dolt AS OF preserves deleted wisps:\n%s", compactCmd.Long)
	}
	if !strings.Contains(compactCmd.Long, "permanent") {
		t.Errorf("compact help must say deletes are permanent:\n%s", compactCmd.Long)
	}
	data, err := os.ReadFile("compact.go")
	if err != nil {
		t.Fatalf("read compact.go: %v", err)
	}
	if strings.Contains(string(data), "AS OF preserves") {
		t.Errorf("compact.go still carries the false 'AS OF preserves history' comment")
	}
}
