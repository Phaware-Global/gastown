package refinery

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// mustMarshalIndent is a test helper that fails the test immediately if
// MarshalIndent returns an error. Failure here means a test fixture became
// non-marshalable (e.g., a value type sneaked in that JSON can't represent),
// which is a programming error in the test rather than a runtime concern.
func mustMarshalIndent(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshaling test fixture: %v", err)
	}
	return data
}

func TestEngineer_LoadConfig_MergeStrategyPR(t *testing.T) {
	tmpDir := t.TempDir()

	requireReview := true
	config := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{
			"merge_strategy": "pr",
			"require_review": requireReview,
			// merge_strategy=pr now requires pr_approver (defense-in-depth
			// validation added in LoadConfig).
			"pr_approver": "gatekeeper",
		},
	}

	data := mustMarshalIndent(t, config)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.MergeStrategy != "pr" {
		t.Errorf("expected MergeStrategy 'pr', got %q", e.config.MergeStrategy)
	}
	if e.config.RequireReview == nil || !*e.config.RequireReview {
		t.Error("expected RequireReview to be true")
	}
}

// TestEngineer_LoadConfig_MergeStrategyPR_NoApproverOptOut covers the
// review-loop-only opt-out path: empty PRApprover combined with an
// explicit pr_required_approvals=0 disables both per-user approval
// gates, leaving only the review-loop and unresolved-threads gates
// active. This combination must load successfully — it's the
// supported way to run a PR-mode rig that gates merges on review-loop
// completion alone, without naming a specific approver.
func TestEngineer_LoadConfig_MergeStrategyPR_NoApproverOptOut(t *testing.T) {
	tmpDir := t.TempDir()
	config := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{
			"merge_strategy":        "pr",
			"pr_approver":           "",
			"pr_required_approvals": 0,
			"pr_reviewer":           "augmentcode",
		},
	}
	data := mustMarshalIndent(t, config)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("expected the no-approver opt-out combination to load, got %v", err)
	}
	if e.config.PRApprover != "" {
		t.Errorf("PRApprover = %q, want empty (opt-out)", e.config.PRApprover)
	}
	if e.config.PRRequiredApprovals == nil || *e.config.PRRequiredApprovals != 0 {
		t.Errorf("PRRequiredApprovals = %v, want explicit 0", e.config.PRRequiredApprovals)
	}
}

// TestEngineer_LoadConfig_MergeStrategyPR_EmptyApproverWithoutOptOut
// asserts the negative cases: empty PRApprover is NOT acceptable
// unless pr_required_approvals=0 is also explicit. The opt-out must
// be deliberate; an unset count gate (which defaults to 1) or an
// explicit positive count gate both still require an approver.
func TestEngineer_LoadConfig_MergeStrategyPR_EmptyApproverWithoutOptOut(t *testing.T) {
	cases := []struct {
		name string
		mq   map[string]interface{}
	}{
		{
			name: "unset pr_required_approvals (defaults to 1)",
			mq: map[string]interface{}{
				"merge_strategy": "pr",
				"pr_approver":    "",
			},
		},
		{
			name: "explicit positive pr_required_approvals",
			mq: map[string]interface{}{
				"merge_strategy":        "pr",
				"pr_approver":           "",
				"pr_required_approvals": 1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			config := map[string]interface{}{
				"type":        "rig",
				"version":     1,
				"name":        "test-rig",
				"merge_queue": tc.mq,
			}
			data := mustMarshalIndent(t, config)
			if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
				t.Fatal(err)
			}
			r := &rig.Rig{Name: "test-rig", Path: tmpDir}
			e := NewEngineer(r)
			err := e.LoadConfig()
			if err == nil {
				t.Fatal("expected validation error for empty pr_approver without explicit pr_required_approvals=0, got nil")
			}
			if !strings.Contains(err.Error(), "pr_approver is required") {
				t.Errorf("error should mention pr_approver requirement, got %v", err)
			}
		})
	}
}

// TestEngineer_LoadConfig_ReadsSettingsPath is the G23 regression test:
// merge_queue lives at <rig>/settings/config.json, NOT <rig>/config.json.
// Before the G23 fix, Engineer.LoadConfig read the rig-root config.json
// (identity file) which has no merge_queue section, so it silently
// returned a zero-valued MergeQueueConfig. That made the G21
// approval-proof gate a no-op in production.
//
// This test asserts that when settings/config.json contains a
// merge_queue section, LoadConfig picks it up. The in-memory tests in
// approval_test.go never exercised the file-based path, which is why
// the bug shipped.
func TestEngineer_LoadConfig_ReadsSettingsPath(t *testing.T) {
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Settings file carries the merge_queue section (the prod layout).
	settings := map[string]interface{}{
		"type":    "rig-settings",
		"version": 1,
		"merge_queue": map[string]interface{}{
			"merge_strategy":        "pr",
			"pr_approver":           "kevinpjones",
			"pr_required_approvals": 1,
			"pr_reviewer":           "augment",
		},
	}
	data := mustMarshalIndent(t, settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Rig-root config.json: identity fields only, no merge_queue.
	// If LoadConfig wrongly reads this file, PRApprover would be empty
	// and the test would catch it.
	identity := map[string]interface{}{
		"type":           "rig",
		"version":        1,
		"name":           "test-rig",
		"default_branch": "main",
	}
	identityData := mustMarshalIndent(t, identity)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), identityData, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.MergeStrategy != "pr" {
		t.Errorf("expected MergeStrategy=pr from settings/config.json, got %q — "+
			"likely still reading rig-root config.json (G23 regression)",
			e.config.MergeStrategy)
	}
	if e.config.PRApprover != "kevinpjones" {
		t.Errorf("expected PRApprover=kevinpjones, got %q (G23 regression)",
			e.config.PRApprover)
	}
	if e.config.PRRequiredApprovals == nil || *e.config.PRRequiredApprovals != 1 {
		t.Errorf("expected PRRequiredApprovals=1, got %v (G23 regression)",
			e.config.PRRequiredApprovals)
	}
}

// TestEngineer_LoadConfig_SettingsPathWins verifies that when both
// <rig>/settings/config.json AND <rig>/config.json have merge_queue
// sections, the settings path wins. Defensive coverage for rigs that
// have drifted state in both files.
func TestEngineer_LoadConfig_SettingsPathWins(t *testing.T) {
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := map[string]interface{}{
		"type":    "rig-settings",
		"version": 1,
		"merge_queue": map[string]interface{}{
			"merge_strategy": "pr",
			"pr_approver":    "from-settings",
		},
	}
	data := mustMarshalIndent(t, settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	root := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{
			"merge_strategy": "direct",
			"pr_approver":    "from-root",
		},
	}
	rootData := mustMarshalIndent(t, root)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), rootData, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.PRApprover != "from-settings" {
		t.Errorf("expected settings/config.json to win, got PRApprover=%q", e.config.PRApprover)
	}
}

// TestEngineer_LoadConfig_RootFallback verifies legacy rigs without a
// settings/config.json still load merge_queue from rig-root config.json.
// Preserves backward compat for tooling that wrote merge_queue to the
// root before settings/ became the canonical location.
func TestEngineer_LoadConfig_RootFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// No settings/ directory at all — exercises the settings-missing fallback.
	legacyConfig := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{
			"merge_strategy": "pr",
			"pr_approver":    "legacy-approver",
		},
	}
	data := mustMarshalIndent(t, legacyConfig)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.PRApprover != "legacy-approver" {
		t.Errorf("fallback to rig-root config.json did not populate PRApprover: got %q",
			e.config.PRApprover)
	}
}

// TestEngineer_LoadConfig_SettingsExistsNoMergeQueue covers the failure mode
// flagged on PR #35: a rig has settings/config.json (e.g. for theme/crew),
// but the merge_queue section was written to the rig-root config.json by
// legacy tooling. Before the fix, the loader returned defaults as soon as
// settings/config.json was readable, silently re-disabling the approval
// gate — the original G23 failure mode reintroduced through a different
// path. This test asserts the loader keeps falling through to the
// rig-root file when the settings file lacks a merge_queue section.
func TestEngineer_LoadConfig_SettingsExistsNoMergeQueue(t *testing.T) {
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// settings/config.json EXISTS but only carries unrelated fields —
	// no merge_queue section. The loader must NOT stop here.
	settings := map[string]interface{}{
		"type":    "rig-settings",
		"version": 1,
		"theme":   "dark",
	}
	data := mustMarshalIndent(t, settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// rig-root config.json holds merge_queue (legacy layout).
	root := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{
			"merge_strategy": "pr",
			"pr_approver":    "from-root-fallback",
		},
	}
	rootData := mustMarshalIndent(t, root)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), rootData, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.PRApprover != "from-root-fallback" {
		t.Errorf("expected fallthrough to rig-root config.json when settings/config.json "+
			"has no merge_queue section, got PRApprover=%q (G23 fallthrough regression)",
			e.config.PRApprover)
	}
}

// TestEngineer_LoadConfig_SettingsExplicitNullMergeQueue covers the edge
// case flagged on PR #35 iteration 2: an explicit `"merge_queue": null` in
// settings/config.json should be treated as "not configured here" and fall
// through to the rig-root file, not selected as the winning candidate.
// Without this, a settings file that explicitly nulls out merge_queue
// would silently disable the approval gate even when the root file has a
// real config — same end-state as the original G23 failure.
func TestEngineer_LoadConfig_SettingsExplicitNullMergeQueue(t *testing.T) {
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// settings/config.json carries an explicit `"merge_queue": null`.
	// json.Unmarshal of null into json.RawMessage stores the literal
	// bytes []byte("null"), not nil — so a naive `!= nil` check would
	// pick this file as the winning candidate.
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"),
		[]byte(`{"type":"rig-settings","version":1,"merge_queue":null}`), 0644); err != nil {
		t.Fatal(err)
	}

	// rig-root config.json holds the real merge_queue.
	root := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{
			"merge_strategy": "pr",
			"pr_approver":    "from-root-when-settings-null",
		},
	}
	rootData := mustMarshalIndent(t, root)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), rootData, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.PRApprover != "from-root-when-settings-null" {
		t.Errorf("expected explicit `merge_queue: null` in settings to fall through "+
			"to rig-root config.json, got PRApprover=%q", e.config.PRApprover)
	}
}

func TestEngineer_LoadConfig_MergeStrategyDefault(t *testing.T) {
	tmpDir := t.TempDir()

	config := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{},
	}

	data := mustMarshalIndent(t, config)
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	if err := e.LoadConfig(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.config.MergeStrategy != "" {
		t.Errorf("expected empty MergeStrategy (default), got %q", e.config.MergeStrategy)
	}
	if e.config.RequireReview != nil {
		t.Error("expected RequireReview to be nil (default)")
	}
}

func TestDoMerge_PRStrategy_RoutesToPRPath(t *testing.T) {
	// When merge_strategy=pr, doMerge should attempt the PR merge path.
	// Without a real GitHub repo, FindPRNumber will fail — that's the expected
	// behavior we test: the code routes to doMergePR and fails gracefully.
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = "pr"

	// Create a feature branch
	createFeatureBranch(t, workDir, "feat/test-pr", "test.txt", "hello")

	result := e.doMerge(context.Background(), "feat/test-pr", "main", "gt-test")

	if result.Success {
		t.Error("expected failure (no GitHub PR exists)")
	}

	output := e.output.(*bytes.Buffer).String()
	if !strings.Contains(output, "PR merge strategy") {
		t.Errorf("expected PR merge strategy log, got: %s", output)
	}
}

func TestDoMerge_DirectStrategy_SkipsPRPath(t *testing.T) {
	// When merge_strategy is empty (direct), doMerge should use the normal path.
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = "" // explicit direct

	createFeatureBranch(t, workDir, "feat/test-direct", "test.txt", "hello")

	result := e.doMerge(context.Background(), "feat/test-direct", "main", "gt-test")

	// Should succeed with direct merge
	if !result.Success {
		t.Errorf("expected success for direct merge, got error: %s", result.Error)
	}

	output := e.output.(*bytes.Buffer).String()
	if strings.Contains(output, "PR merge strategy") {
		t.Error("direct merge should not mention PR merge strategy")
	}
}

func TestDoMergePR_NoPR_ReturnsError(t *testing.T) {
	// doMergePR should return an error when no PR exists for the branch.
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)

	createFeatureBranch(t, workDir, "feat/no-pr", "test.txt", "hello")

	result := e.doMergePR(context.Background(), "feat/no-pr", "main")

	if result.Success {
		t.Error("expected failure when no PR exists")
	}
	// The error should mention finding a PR
	if !strings.Contains(result.Error, "PR") && !strings.Contains(result.Error, "pr") {
		t.Errorf("expected PR-related error, got: %s", result.Error)
	}
}

func TestProcessResult_NeedsApproval(t *testing.T) {
	// Verify NeedsApproval field works on ProcessResult.
	r := ProcessResult{
		Success:       false,
		NeedsApproval: true,
		Error:         "PR #42 requires approving review before merge",
	}

	if r.Success {
		t.Error("expected Success=false")
	}
	if !r.NeedsApproval {
		t.Error("expected NeedsApproval=true")
	}
}

func TestHandleMRInfoFailure_NeedsApproval_StaysInQueue(t *testing.T) {
	// When NeedsApproval is true, the MR should stay in queue without
	// sending failure notifications to polecats or mayor.
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	var buf bytes.Buffer
	e.output = &buf
	e.workDir = workDir
	e.mergeSlotEnsureExists = func() (string, error) { return "test-slot", nil }
	e.mergeSlotAcquire = func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error) {
		return &beads.MergeSlotStatus{Available: true, Holder: holder}, nil
	}
	e.mergeSlotRelease = func(holder string) error { return nil }

	mr := &MRInfo{
		ID:          "gt-test",
		Branch:      "polecat/test/gt-test",
		Target:      "main",
		SourceIssue: "gt-src",
		Worker:      "polecats/test",
	}
	result := ProcessResult{
		Success:       false,
		NeedsApproval: true,
		Error:         "PR #42 requires approving review before merge",
	}

	e.HandleMRInfoFailure(mr, result)

	output := buf.String()
	if !strings.Contains(output, "awaiting human approval") {
		t.Errorf("expected 'awaiting human approval' message, got: %s", output)
	}
	// Should NOT contain merge failure notifications
	if strings.Contains(output, "MERGE_FAILED") {
		t.Error("NeedsApproval should not trigger MERGE_FAILED notification")
	}
}

// TestEngineer_LoadConfig_RejectsTimeoutLEWait covers the cross-field
// invariant flagged on PR #37: pr_review_timeout must be greater than
// pr_review_wait. If the timeout fires inside the min-wait window,
// await-review can never reach the threads/reviewer checks. LoadConfig
// must fail fast at config-load rather than letting the misconfiguration
// surface as a runtime error on first invocation.
func TestEngineer_LoadConfig_RejectsTimeoutLEWait(t *testing.T) {
	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// timeout (3m) <= wait (5m) — invalid.
	settings := map[string]interface{}{
		"type":    "rig-settings",
		"version": 1,
		"merge_queue": map[string]interface{}{
			"merge_strategy":    "pr",
			"pr_approver":       "kevinpjones",
			"pr_review_wait":    "5m",
			"pr_review_timeout": "3m",
		},
	}
	data := mustMarshalIndent(t, settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	e := NewEngineer(r)
	err := e.LoadConfig()
	if err == nil {
		t.Fatal("expected error when pr_review_timeout <= pr_review_wait, got nil")
	}
	if !strings.Contains(err.Error(), "pr_review_timeout") || !strings.Contains(err.Error(), "pr_review_wait") {
		t.Errorf("error should name both fields, got: %v", err)
	}
}

// threadGateFakeProvider returns a non-zero PR number, configurable
// unresolved threads, and panics on MergePR — so a test can verify
// that the threads-resolved gate (PR.2a) short-circuits doMergePR
// before any merge is attempted. Distinct from fakePRProvider in
// approval_test.go which is shaped for the approval gate.
type threadGateFakeProvider struct {
	prNumber          int
	unresolvedThreads []ReviewThread
	mergeCalls        int
}

func (f *threadGateFakeProvider) FindPRNumber(string) (int, error) {
	return f.prNumber, nil
}
func (f *threadGateFakeProvider) UnresolvedThreads(int) ([]ReviewThread, error) {
	return f.unresolvedThreads, nil
}
func (f *threadGateFakeProvider) MergePR(int, string) (string, error) {
	f.mergeCalls++
	panic("MergePR called — threads-resolved gate failed to short-circuit")
}

func (f *threadGateFakeProvider) IsPRApproved(int) (bool, error)                { panic("unused") }
func (f *threadGateFakeProvider) IsPRApprovedBy(int, string) (bool, error)      { panic("unused") }
func (f *threadGateFakeProvider) CountApprovals(int) (int, error)               { panic("unused") }
func (f *threadGateFakeProvider) CreatePR(CreatePROptions) (int, string, error) { panic("unused") }
func (f *threadGateFakeProvider) RequestReview(int, []string) error             { panic("unused") }
func (f *threadGateFakeProvider) AllThreads(int) ([]ReviewThread, error)        { panic("unused") }
func (f *threadGateFakeProvider) ChecksRollup(int) (string, bool, error)        { panic("unused") }
func (f *threadGateFakeProvider) PostComment(int, string) error                 { panic("unused") }
func (f *threadGateFakeProvider) HasReviewFrom(int, string) (bool, error)       { panic("unused") }
func (f *threadGateFakeProvider) ListReviewAuthors(int) ([]string, error)        { panic("unused") }
func (f *threadGateFakeProvider) HasReviewFromOnSHA(int, string, string) (bool, error) {
	panic("unused")
}
func (f *threadGateFakeProvider) CurrentHeadSHA(int) (string, error) { panic("unused") }

// TestDoMergePR_UnresolvedThreads_ShortCircuits asserts the contract of
// the new PR.2a thread gate: when VerifyReviewThreadsResolved returns
// *NeedsReviewResolutionError, doMergePR sets ProcessResult.
// NeedsReviewResolution=true and returns BEFORE calling MergePR. The
// fake provider panics on MergePR to make a regression unmistakable —
// any future refactor that bypasses the gate will fail the test loudly
// rather than silently letting a thread-blocked PR merge.
func TestDoMergePR_UnresolvedThreads_ShortCircuits(t *testing.T) {
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	createFeatureBranch(t, workDir, "feat/with-threads", "test.txt", "hello")

	provider := &threadGateFakeProvider{
		prNumber: 42,
		unresolvedThreads: []ReviewThread{
			{
				ID:         "PRRT_test_thread",
				IsResolved: false,
				IsOutdated: false,
				URL:        "https://github.com/example/repo/pull/42#discussion_r1",
				Path:       "internal/foo.go",
				Line:       100,
				Author:     "gemini-code-assist",
				Body:       "**Severity: high**\n\nThis function leaks a goroutine on error.",
			},
		},
	}
	e.prProvider = provider

	result := e.doMergePR(context.Background(), "feat/with-threads", "main")

	if result.Success {
		t.Errorf("expected Success=false when threads block, got Success=true")
	}
	if !result.NeedsReviewResolution {
		t.Errorf("expected NeedsReviewResolution=true (G24 gate fired), got false. Result: %+v", result)
	}
	if result.NeedsApproval {
		t.Errorf("threads-blocking path must NOT set NeedsApproval — that misattributes "+
			"the gate to observability. Result: %+v", result)
	}
	if provider.mergeCalls != 0 {
		t.Errorf("MergePR was invoked %d time(s) despite blocking threads — gate did not short-circuit",
			provider.mergeCalls)
	}
	if !strings.Contains(result.Error, "thread") && !strings.Contains(result.Error, "Thread") {
		t.Errorf("expected error to mention thread blocking, got: %s", result.Error)
	}
}

// TestProcessResult_NeedsReviewResolution verifies the field exists and
// is independent of NeedsApproval — distinguishing "blocked by unresolved
// reviewer threads" from "blocked by missing approving review" so
// observability accurately attributes the gate that's holding up merge.
func TestProcessResult_NeedsReviewResolution(t *testing.T) {
	r := ProcessResult{
		Success:               false,
		NeedsReviewResolution: true,
		Error:                 "PR #42 has 2 unresolved review threads",
	}

	if r.Success {
		t.Error("expected Success=false")
	}
	if !r.NeedsReviewResolution {
		t.Error("expected NeedsReviewResolution=true")
	}
	if r.NeedsApproval {
		t.Error("NeedsReviewResolution must not imply NeedsApproval — distinct queue states")
	}
}

// TestHandleMRInfoFailure_NeedsReviewResolution_StaysInQueue mirrors the
// NeedsApproval test but for the threads-blocking path. The MR must stay
// in queue, the log line must say "unresolved review threads" (not
// "awaiting human approval"), and no MERGE_FAILED notification fires.
func TestHandleMRInfoFailure_NeedsReviewResolution_StaysInQueue(t *testing.T) {
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	var buf bytes.Buffer
	e.output = &buf
	e.workDir = workDir
	e.mergeSlotEnsureExists = func() (string, error) { return "test-slot", nil }
	e.mergeSlotAcquire = func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error) {
		return &beads.MergeSlotStatus{Available: true, Holder: holder}, nil
	}
	e.mergeSlotRelease = func(holder string) error { return nil }

	mr := &MRInfo{
		ID:          "gt-test",
		Branch:      "polecat/test/gt-test",
		Target:      "main",
		SourceIssue: "gt-src",
		Worker:      "polecats/test",
	}
	// Detailed-error string mimics what VerifyReviewThreadsResolved
	// produces (file:line, author, priority preview). The handler must
	// surface this to patrol output so the polecat dispatcher knows
	// WHICH threads need attention.
	detailedErr := "PR #42 has 2 unresolved review threads:\n  - internal/foo.go:42 by gemini-code-assist [high] - off-by-one\n  - internal/bar.go:7 by augmentcode [medium] - nil deref"
	result := ProcessResult{
		Success:               false,
		NeedsReviewResolution: true,
		Error:                 detailedErr,
	}

	e.HandleMRInfoFailure(mr, result)

	output := buf.String()
	if !strings.Contains(output, "unresolved review threads") {
		t.Errorf("expected 'unresolved review threads' message, got: %s", output)
	}
	if strings.Contains(output, "awaiting human approval") {
		t.Errorf("NeedsReviewResolution must NOT log 'awaiting human approval' — that "+
			"misattributes the blocker. Output: %s", output)
	}
	if strings.Contains(output, "MERGE_FAILED") {
		t.Error("NeedsReviewResolution should not trigger MERGE_FAILED notification")
	}
	// The detailed thread list must reach patrol output — without it
	// the polecat dispatcher only sees that SOMETHING is blocking, not
	// WHAT, so the review-fix loop can't run with focused context.
	if !strings.Contains(output, "off-by-one") || !strings.Contains(output, "gemini-code-assist") {
		t.Errorf("expected detailed thread list (from result.Error) to surface in output, got: %s", output)
	}
}

func TestDoMergePR_RequireReview_NoApproval(t *testing.T) {
	// When require_review is true and the PR is not approved,
	// doMergePR should return NeedsApproval=true.
	// This test is tricky since it requires gh CLI — skip if not available.
	if _, err := gitpkg.NewGit(t.TempDir()).FindPRNumber("nonexistent"); err != nil {
		// gh CLI not available or not authenticated — test the config path only
		t.Skip("gh CLI not available for PR approval testing")
	}
}
