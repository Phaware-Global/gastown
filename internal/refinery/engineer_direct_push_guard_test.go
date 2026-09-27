package refinery

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/testutil"
)

// Tests for the direct-push guard, driven through the entry points a
// merge actually takes (ProcessMRInfo, ProcessBatch) rather than doMerge
// alone. Each one asserts the only thing that matters: nothing reaches the
// target branch — or a submodule's default branch — on a merge_strategy=pr
// rig, and the refusal is escalated once and then parked.

// wantParkLabel is the label that parks a refused MR out of the ready queue.
const wantParkLabel = "gt:review-pr-missing"

type recordedEscalation struct {
	severity, source, fingerprint, message string
}

// recordEscalations swaps the engineer's escalate seam for a recorder. Tests
// that refuse MRs with made-up IDs also need isolatedBeads, so parking them
// cannot reach a real beads database.
func recordEscalations(e *Engineer) *[]recordedEscalation {
	var got []recordedEscalation
	e.escalate = func(severity, source, fingerprint, message string) error {
		got = append(got, recordedEscalation{severity, source, fingerprint, message})
		return nil
	}
	return &got
}

// isolatedBeads points the engineer at an empty, isolated beads directory:
// updates to made-up MR IDs fail harmlessly instead of routing to a live rig.
func isolatedBeads(t *testing.T, e *Engineer) {
	t.Helper()
	e.beads = beads.NewIsolated(t.TempDir())
}

// remoteRef returns the SHA that origin's ref points at, read from the remote
// itself so a local-only reset can't mask a push.
func remoteRef(t *testing.T, dir, ref string) string {
	t.Helper()
	out := run(t, dir, "git", "ls-remote", "origin", ref)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("origin has no %s (ls-remote output %q)", ref, out)
	}
	return fields[0]
}

// newDoltBeads returns a real, isolated beads store. bd init can return its
// anonymous-metrics notice as an error even though it succeeded, so that
// notice must not be mistaken for "bd unavailable" and turn the test into a
// silent skip.
func newDoltBeads(t *testing.T) *beads.Beads {
	t.Helper()
	testutil.RequireDoltContainer(t)
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil {
		t.Fatalf("parsing dolt container port: %v", err)
	}
	b := beads.NewIsolatedWithPort(t.TempDir(), port)
	if err := b.Init("gt"); err != nil && !strings.Contains(err.Error(), "anonymous usage metrics") {
		t.Skipf("bd init unavailable in test environment: %v", err)
	}
	return b
}

// createMRBead creates an MR bead and closes it when the test ends: the Dolt
// container is shared, so a bead left open here shows up in another test's
// ready queue.
func createMRBead(t *testing.T, b *beads.Beads, description string) *beads.Issue {
	t.Helper()
	mr, err := b.Create(beads.CreateOptions{
		Title:       "Merge: gt-test",
		Labels:      []string{"gt:merge-request"},
		Description: description,
	})
	if err != nil {
		t.Fatalf("create MR bead: %v", err)
	}
	t.Cleanup(func() { _ = b.CloseWithReason("test cleanup", mr.ID) })
	return mr
}

// prRigCase describes how the engineer's in-memory config relates to the rig's
// settings on disk. "stale" is the incident shape (snapshot never loaded, disk
// says pr); "loaded" is a correctly configured PR rig.
type prRigCase struct {
	name           string
	memoryStrategy string
}

var prRigCases = []prRigCase{
	{"stale-config", ""},
	{"loaded-config", "pr"},
}

func TestProcessMRInfo_PRRig_NoPushToTarget(t *testing.T) {
	for _, tc := range prRigCases {
		t.Run(tc.name, func(t *testing.T) {
			workDir, g, _ := testGitRepo(t)
			e := newTestEngineer(t, workDir, g)
			e.config.MergeStrategy = tc.memoryStrategy
			createFeatureBranch(t, workDir, "feat/one", "one.txt", "1")
			writeRigSettingsMergeStrategy(t, workDir, "pr")
			before := remoteRef(t, workDir, "main")

			result := e.ProcessMRInfo(context.Background(), makeMR("gt-mr-1", "feat/one", "main"))

			if result.Success {
				t.Fatal("ProcessMRInfo merged on a merge_strategy=pr rig with no review_pr")
			}
			if after := remoteRef(t, workDir, "main"); after != before {
				t.Fatalf("origin/main moved %s -> %s", before, after)
			}
		})
	}
}

func TestProcessBatch_PRRig_MultiMR_NoPushToTarget(t *testing.T) {
	for _, tc := range prRigCases {
		t.Run(tc.name, func(t *testing.T) {
			workDir, g, _ := testGitRepo(t)
			e := newTestEngineer(t, workDir, g)
			e.config.MergeStrategy = tc.memoryStrategy
			escalations := recordEscalations(e)
			isolatedBeads(t, e)
			createFeatureBranch(t, workDir, "feat/a", "a.txt", "a")
			createFeatureBranch(t, workDir, "feat/b", "b.txt", "b")
			writeRigSettingsMergeStrategy(t, workDir, "pr")
			before := remoteRef(t, workDir, "main")
			localBefore := run(t, workDir, "git", "rev-parse", "main")

			// One MR carries a review_pr, one does not: on a PR rig a direct
			// push is refused for both, since it bypasses the PR merge API
			// (branch protection, approvals, audit trail) either way.
			withPR := makeMR("gt-mr-b", "feat/b", "main")
			withPR.ReviewPR = 12
			batch := []*MRInfo{makeMR("gt-mr-a", "feat/a", "main"), withPR}

			result := e.ProcessBatch(context.Background(), batch, "main", &BatchConfig{MaxBatchSize: 5})

			if after := remoteRef(t, workDir, "main"); after != before {
				t.Fatalf("origin/main moved %s -> %s: batch pushed to the target on a PR rig", before, after)
			}
			if len(result.Merged) != 0 || result.MergeCommit != "" {
				t.Errorf("batch reported a merge: merged=%v commit=%q", mrIDs(result.Merged), result.MergeCommit)
			}
			if result.Error == nil {
				t.Error("batch refusal must surface as result.Error")
			}
			if local := run(t, workDir, "git", "rev-parse", "main"); local != localBefore {
				t.Errorf("local main left at %s, want %s (no stray squash commits)", local, localBefore)
			}
			assertOneEscalationPerMR(t, *escalations, "gt-mr-a", "gt-mr-b")
		})
	}
}

// A batch that shrinks to one surviving MR (verifyAndPush) and the degenerate
// one-MR batch (processSingleMR) must be refused and escalated too.
func TestProcessBatch_PRRig_SingleMR_RefusedAndEscalated(t *testing.T) {
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = ""
	escalations := recordEscalations(e)
	isolatedBeads(t, e)
	createFeatureBranch(t, workDir, "feat/solo", "solo.txt", "s")
	writeRigSettingsMergeStrategy(t, workDir, "pr")
	before := remoteRef(t, workDir, "main")

	result := e.ProcessBatch(context.Background(), []*MRInfo{makeMR("gt-mr-solo", "feat/solo", "main")}, "main", nil)

	if after := remoteRef(t, workDir, "main"); after != before {
		t.Fatalf("origin/main moved %s -> %s", before, after)
	}
	if len(result.Merged) != 0 {
		t.Errorf("merged=%v, want none", mrIDs(result.Merged))
	}
	assertOneEscalationPerMR(t, *escalations, "gt-mr-solo")
}

// The batch push helpers themselves must refuse: they are the last thing
// between a stacked local main and origin, so they cannot rely on the caller
// having checked first.
func TestBatchPushHelpers_PRRig_RefuseToPush(t *testing.T) {
	for _, tc := range []struct {
		name string
		push func(e *Engineer, stacked []*MRInfo) *BatchResult
	}{
		{"fastForwardBatch", func(e *Engineer, stacked []*MRInfo) *BatchResult {
			return e.fastForwardBatch(context.Background(), stacked, "main", &BatchResult{})
		}},
		{"verifyAndPush", func(e *Engineer, stacked []*MRInfo) *BatchResult {
			return e.verifyAndPush(context.Background(), stacked, "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir, g, _ := testGitRepo(t)
			e := newTestEngineer(t, workDir, g)
			e.config.MergeStrategy = ""
			recordEscalations(e)
			isolatedBeads(t, e)
			createFeatureBranch(t, workDir, "feat/x", "x.txt", "x")
			createFeatureBranch(t, workDir, "feat/y", "y.txt", "y")
			writeRigSettingsMergeStrategy(t, workDir, "pr")
			before := remoteRef(t, workDir, "main")
			localBefore := run(t, workDir, "git", "rev-parse", "main")

			batch := []*MRInfo{makeMR("gt-mr-x", "feat/x", "main"), makeMR("gt-mr-y", "feat/y", "main")}
			stacked, _, err := e.BuildRebaseStack(context.Background(), batch, "main")
			if err != nil || len(stacked) != 2 {
				t.Fatalf("BuildRebaseStack: stacked=%d err=%v", len(stacked), err)
			}

			result := tc.push(e, stacked)

			if after := remoteRef(t, workDir, "main"); after != before {
				t.Fatalf("origin/main moved %s -> %s", before, after)
			}
			if len(result.Merged) != 0 || result.Error == nil {
				t.Errorf("want refusal, got merged=%v err=%v", mrIDs(result.Merged), result.Error)
			}
			if local := run(t, workDir, "git", "rev-parse", "main"); local != localBefore {
				t.Errorf("local main left at %s after refusal, want reset to %s", local, localBefore)
			}
		})
	}
}

// Control: the guard must not fire on a rig that genuinely merges directly.
func TestProcessBatch_DirectRig_MultiMR_StillMerges(t *testing.T) {
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = ""
	escalations := recordEscalations(e)
	isolatedBeads(t, e)
	createFeatureBranch(t, workDir, "feat/a", "a.txt", "a")
	createFeatureBranch(t, workDir, "feat/b", "b.txt", "b")
	writeRigSettingsMergeStrategy(t, workDir, "direct")
	before := remoteRef(t, workDir, "main")

	result := e.ProcessBatch(context.Background(),
		[]*MRInfo{makeMR("gt-mr-a", "feat/a", "main"), makeMR("gt-mr-b", "feat/b", "main")},
		"main", &BatchConfig{MaxBatchSize: 5})

	if result.Error != nil || len(result.Merged) != 2 {
		t.Fatalf("direct rig batch: merged=%v err=%v", mrIDs(result.Merged), result.Error)
	}
	if remoteRef(t, workDir, "main") == before {
		t.Error("direct rig batch did not push")
	}
	if len(*escalations) != 0 {
		t.Errorf("direct rig escalated: %+v", *escalations)
	}
}

// assertOneEscalationPerMR checks each MR was escalated exactly once, HIGH,
// under a fingerprint that is stable and specific to that MR so a re-poll
// collapses onto the same escalation.
func assertOneEscalationPerMR(t *testing.T, got []recordedEscalation, mrIDs ...string) {
	t.Helper()
	if len(got) != len(mrIDs) {
		t.Fatalf("want %d escalations (one per MR %v), got %d: %+v", len(mrIDs), mrIDs, len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range mrIDs {
		var match *recordedEscalation
		for i := range got {
			if strings.Contains(got[i].message, id) {
				match = &got[i]
			}
		}
		if match == nil {
			t.Errorf("no escalation names MR %s: %+v", id, got)
			continue
		}
		if match.severity != "HIGH" {
			t.Errorf("MR %s escalated at %q, want HIGH", id, match.severity)
		}
		if match.fingerprint == "" || !strings.Contains(match.fingerprint, id) {
			t.Errorf("MR %s escalation fingerprint %q must be non-empty and name the MR", id, match.fingerprint)
		}
		if seen[match.fingerprint] {
			t.Errorf("fingerprint %q shared between MRs", match.fingerprint)
		}
		seen[match.fingerprint] = true
	}
}

// testGitRepoWithSubmodule is testGitRepo plus a submodule at libs/sub, and a
// branch feat/sub whose only change is a new submodule commit that exists
// locally but has not been pushed to the submodule's remote.
func testGitRepoWithSubmodule(t *testing.T) (workDir string, g *gitpkg.Git, subRemote, newSubSHA string) {
	t.Helper()
	// Submodule clones over the local file transport, which git blocks by default.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	tmp := t.TempDir()
	subRemote = filepath.Join(tmp, "sub-remote.git")
	run(t, tmp, "git", "init", "--bare", "--initial-branch=main", subRemote)
	subSeed := filepath.Join(tmp, "sub-seed")
	run(t, tmp, "git", "clone", subRemote, subSeed)
	run(t, subSeed, "git", "config", "user.email", "test@test.com")
	run(t, subSeed, "git", "config", "user.name", "Test")
	writeFile(t, subSeed, "lib.go", "package lib\n")
	run(t, subSeed, "git", "add", ".")
	run(t, subSeed, "git", "commit", "-m", "sub initial")
	run(t, subSeed, "git", "push", "origin", "HEAD:main")

	bare := filepath.Join(tmp, "origin.git")
	workDir = filepath.Join(tmp, "work")
	run(t, tmp, "git", "init", "--bare", "--initial-branch=main", bare)
	run(t, tmp, "git", "clone", bare, workDir)
	run(t, workDir, "git", "config", "user.email", "test@test.com")
	run(t, workDir, "git", "config", "user.name", "Test")
	run(t, workDir, "git", "checkout", "-b", "main")
	writeFile(t, workDir, "README.md", "# Test\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "initial commit")
	run(t, workDir, "git", "submodule", "add", subRemote, "libs/sub")
	run(t, workDir, "git", "commit", "-m", "add submodule")
	run(t, workDir, "git", "push", "-u", "origin", "main")

	subDir := filepath.Join(workDir, "libs", "sub")
	run(t, subDir, "git", "config", "user.email", "test@test.com")
	run(t, subDir, "git", "config", "user.name", "Test")
	run(t, workDir, "git", "checkout", "-b", "feat/sub", "main")
	writeFile(t, subDir, "new.go", "package lib\n// new\n")
	run(t, subDir, "git", "add", ".")
	run(t, subDir, "git", "commit", "-m", "sub new commit")
	newSubSHA = run(t, subDir, "git", "rev-parse", "HEAD")
	run(t, workDir, "git", "add", "libs/sub")
	run(t, workDir, "git", "commit", "-m", "bump submodule")
	run(t, workDir, "git", "checkout", "main")

	return workDir, gitpkg.NewGit(workDir), subRemote, newSubSHA
}

func remoteHasObject(t *testing.T, remote, sha string) bool {
	t.Helper()
	return exec.Command("git", "-C", remote, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// The guard has to run before ANY push. A refused merge that has already
// pushed the branch's submodule commits to the submodule's default branch has
// published unreviewed code.
func TestPRRig_SubmoduleChange_NoSubmodulePush(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(e *Engineer) ProcessResult
	}{
		{"ProcessMRInfo", func(e *Engineer) ProcessResult {
			return e.ProcessMRInfo(context.Background(), makeMR("gt-mr-sub", "feat/sub", "main"))
		}},
		{"ProcessBatch-single", func(e *Engineer) ProcessResult {
			r := e.ProcessBatch(context.Background(), []*MRInfo{makeMR("gt-mr-sub", "feat/sub", "main")}, "main", nil)
			return ProcessResult{Success: len(r.Merged) > 0}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir, g, subRemote, newSubSHA := testGitRepoWithSubmodule(t)
			e := newTestEngineer(t, workDir, g)
			e.config.MergeStrategy = ""
			recordEscalations(e)
			isolatedBeads(t, e)
			writeRigSettingsMergeStrategy(t, workDir, "pr")
			before := remoteRef(t, workDir, "main")

			result := tc.run(e)

			if result.Success {
				t.Fatal("merged on a PR rig")
			}
			if remoteHasObject(t, subRemote, newSubSHA) {
				t.Errorf("submodule commit %s was pushed to the submodule remote before the guard ran", newSubSHA[:8])
			}
			if after := remoteRef(t, workDir, "main"); after != before {
				t.Errorf("origin/main moved %s -> %s", before, after)
			}
		})
	}
}

// Control: a direct rig still pushes the submodule commit.
func TestDirectRig_SubmoduleChange_StillPushesSubmodule(t *testing.T) {
	workDir, g, subRemote, newSubSHA := testGitRepoWithSubmodule(t)
	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = ""
	writeRigSettingsMergeStrategy(t, workDir, "direct")

	result := e.ProcessMRInfo(context.Background(), makeMR("gt-mr-sub", "feat/sub", "main"))

	if !result.Success {
		t.Fatalf("direct rig merge failed: %s", result.Error)
	}
	if !remoteHasObject(t, subRemote, newSubSHA) {
		t.Error("direct rig did not push the submodule commit")
	}
}

// Every push in this package that can land on the target branch, or on a
// submodule's default branch, must be issued by one of the guarded helpers.
// A new caller that reaches for e.git.Push directly is the bypass this test
// exists to catch — the guard cannot protect a path that does not call it.
func TestRawPushesOnlyInsideGuardedHelpers(t *testing.T) {
	allowed := map[string]bool{
		"pushTargetBranch":    true,
		"pushSubmoduleCommit": true,
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing package: %v", err)
	}
	for _, pkg := range pkgs {
		for fname, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || allowed[fn.Name.Name] {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || (sel.Sel.Name != "Push" && sel.Sel.Name != "PushSubmoduleCommit") {
						return true
					}
					if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "git" {
						t.Errorf("%s: %s calls e.git.%s directly; route it through a guarded push helper",
							fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name)
					}
					return true
				})
			}
			_ = fname
		}
	}
}

// review_pr, once recorded on an MR bead, is never rewritten by a branch-name
// lookup: the tracked PR is what the review-fix loop and the gates key off.
func TestPopulateReviewPR_NeverOverwritesExisting(t *testing.T) {
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.beads = newDoltBeads(t)

	mr := createMRBead(t, e.beads, "branch: feat/x\ntarget: main\nsource_issue: gt-test\nreview_pr: 5")

	_ = e.populateReviewPR(mr.ID, 77) // branch lookup found a different PR

	got, err := e.beads.Show(mr.ID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if fields := beads.ParseMRFields(got); fields == nil || fields.ReviewPR != 5 {
		t.Fatalf("review_pr = %+v, want it left at 5", fields)
	}
}

// A refused MR is escalated once and parked: it must not re-enter the ready
// queue and re-file a HIGH escalation on every poll.
func TestReviewPRMissing_EscalatesOnceAndParks(t *testing.T) {
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.beads = newDoltBeads(t)
	e.config.MergeStrategy = ""
	escalations := recordEscalations(e)
	createFeatureBranch(t, workDir, "feat/parked", "p.txt", "p")
	writeRigSettingsMergeStrategy(t, workDir, "pr")

	mrBead := createMRBead(t, e.beads, "branch: feat/parked\ntarget: main\nsource_issue: gt-test")
	before := remoteRef(t, workDir, "main")

	polls := 4
	for i := 0; i < polls; i++ {
		ready, err := e.ListReadyMRs()
		if err != nil {
			t.Fatalf("poll %d: ListReadyMRs: %v", i, err)
		}
		for _, mr := range ready {
			if result := e.ProcessMRInfo(context.Background(), mr); !result.Success {
				e.HandleMRInfoFailure(mr, result)
			}
		}
	}

	if after := remoteRef(t, workDir, "main"); after != before {
		t.Fatalf("origin/main moved %s -> %s", before, after)
	}
	var mine []recordedEscalation
	for _, esc := range *escalations {
		if strings.Contains(esc.message, mrBead.ID) {
			mine = append(mine, esc)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("want exactly 1 escalation for %s across %d polls, got %d: %+v", mrBead.ID, polls, len(mine), mine)
	}
	if fp := mine[0].fingerprint; fp == "" || !strings.Contains(fp, mrBead.ID) {
		t.Errorf("fingerprint %q must be non-empty and name the MR %s", fp, mrBead.ID)
	}
	parked, err := e.beads.Show(mrBead.ID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !beads.HasLabel(parked, wantParkLabel) {
		t.Errorf("MR labels = %v, want %q so it is parked", parked.Labels, wantParkLabel)
	}
	if parked.Status != "open" {
		t.Errorf("parked MR status = %q; it must stay open for a human to resolve", parked.Status)
	}
}
