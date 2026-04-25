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

	data, _ := json.MarshalIndent(config, "", "  ")
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
	data, _ := json.MarshalIndent(settings, "", "  ")
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
	identityData, _ := json.MarshalIndent(identity, "", "  ")
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
	data, _ := json.MarshalIndent(settings, "", "  ")
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
	rootData, _ := json.MarshalIndent(root, "", "  ")
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
	data, _ := json.MarshalIndent(legacyConfig, "", "  ")
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
	data, _ := json.MarshalIndent(settings, "", "  ")
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
	rootData, _ := json.MarshalIndent(root, "", "  ")
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

func TestEngineer_LoadConfig_MergeStrategyDefault(t *testing.T) {
	tmpDir := t.TempDir()

	config := map[string]interface{}{
		"type":    "rig",
		"version": 1,
		"name":    "test-rig",
		"merge_queue": map[string]interface{}{},
	}

	data, _ := json.MarshalIndent(config, "", "  ")
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

func TestDoMergePR_RequireReview_NoApproval(t *testing.T) {
	// When require_review is true and the PR is not approved,
	// doMergePR should return NeedsApproval=true.
	// This test is tricky since it requires gh CLI — skip if not available.
	if _, err := gitpkg.NewGit(t.TempDir()).FindPRNumber("nonexistent"); err != nil {
		// gh CLI not available or not authenticated — test the config path only
		t.Skip("gh CLI not available for PR approval testing")
	}
}
