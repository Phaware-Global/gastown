package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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

// fakeWispSQL answers the five listWispsWith queries from canned rows, keyed
// by the table each query reads. Any bd subcommand other than `sql --json`
// (notably `list`, which never returns wisps) yields an empty array, as the
// real bd does.
func fakeWispSQL(wisps, labels, agents, comments, parents string) wispSQLRunner {
	return func(args ...string) ([]byte, error) {
		if len(args) != 3 || args[0] != "sql" || args[1] != "--json" {
			return []byte("[]"), nil
		}
		q := args[2]
		switch {
		case strings.Contains(q, "FROM wisp_labels"):
			return []byte(labels), nil
		case strings.Contains(q, "FROM labels"):
			return []byte(agents), nil
		case strings.Contains(q, "FROM wisp_comments"):
			return []byte(comments), nil
		case strings.Contains(q, "FROM wisp_dependencies"):
			return []byte(parents), nil
		case strings.Contains(q, "FROM wisps"):
			return []byte(wisps), nil
		}
		return nil, os.ErrInvalid
	}
}

func TestListWispsReadsWispsTable(t *testing.T) {
	wisps := `[
	 {"id":"gt-wisp-a","title":"patrol","status":"open","issue_type":"task","wisp_type":"patrol","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"},
	 {"id":"gt-wisp-b","title":"step","status":"open","issue_type":"task","wisp_type":"","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"},
	 {"id":"gt-wisp-c","title":"agent via type","status":"open","issue_type":"agent","wisp_type":"","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"},
	 {"id":"gt-wisp-d","title":"agent via wisp_labels","status":"open","issue_type":"task","wisp_type":"","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"},
	 {"id":"gt-wisp-e","title":"agent via labels","status":"open","issue_type":"task","wisp_type":"","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}
	]`
	got, err := listWispsWith(fakeWispSQL(wisps,
		`[{"issue_id":"gt-wisp-a","label":"keep"},{"issue_id":"gt-wisp-d","label":"gt:agent"}]`,
		`[{"issue_id":"gt-wisp-e"}]`,
		`[{"issue_id":"gt-wisp-a","n":2}]`,
		`[{"issue_id":"gt-wisp-b","parent_id":"gt-wisp-p","parent_open":1}]`))
	if err != nil {
		t.Fatalf("listWispsWith: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("scanned %d wisps, want 2 (a, b); agent beads must be excluded", len(got))
	}
	a, b := got[0], got[1]
	if a.ID != "gt-wisp-a" || a.WispType != "patrol" || a.CommentCount != 2 || !hasKeepLabel(a) || !a.Ephemeral {
		t.Errorf("wisp a mis-decoded: %+v", a)
	}
	if b.ID != "gt-wisp-b" || b.Parent != "gt-wisp-p" || !b.ParentOpen {
		t.Errorf("wisp b parent mis-decoded: %+v", b)
	}
}

// TestListWispsDoesNotDependOnBdList pins the gt-jux2 regression: bd list
// returns no wisps, so a scan built on it saw zero rows forever.
func TestListWispsDoesNotDependOnBdList(t *testing.T) {
	row := `[{"id":"gt-wisp-a","title":"t","status":"open","issue_type":"task","wisp_type":"","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
	run := fakeWispSQL(row, "[]", "[]", "[]", "[]")
	guarded := func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			t.Errorf("listWisps must not call bd list: %v", args)
		}
		return run(args...)
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
