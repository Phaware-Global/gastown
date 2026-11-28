package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/util"
)

var reviewerRunCmd = &cobra.Command{
	Use:   "run [perspective ...]",
	Short: "Run the perspective passes and record a receipt for each",
	Long: `Run one headless agent per perspective, in parallel, on the checked-out head.

Each pass executes the same prompt 'gt reviewer prompt' prints. The command
writes the pass's raw perspective-<name>.json plus receipt-<name>.json, which
records the pass id, prompt and result hashes, and start and end times, under
<rig>/reviewer/passes/pr<N>/. 'consolidate' and 'post'
accept only results with a matching receipt.

With no arguments every enabled perspective runs; name one to re-run it. A pass
can take longer than a foreground shell timeout, so run this in the background
and wait for it to exit.`,
	RunE: runReviewerRun,
}

// reviewerBaseSHA returns the merge-base of a head with the PR's base branch. A
// variable so tests can run without a hosted PR.
var reviewerBaseSHA = resolveReviewBaseSHA

// reviewerEnabledPerspectives returns the perspectives the rig requires a review
// to cover. A variable so tests can run outside a workspace.
var reviewerEnabledPerspectives = func() ([]string, error) {
	townRoot, _, rigPath, reviewCfg, err := loadRigReviewConfig()
	if err != nil {
		return nil, err
	}
	failSilent := reviewCfg != nil && reviewCfg.FailSilentPerspectives
	resolved, _, err := reviewer.ResolvePerspectives(townRoot, rigPath, reviewCfg.GetPerspectives(), failSilent)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(resolved))
	for i, rp := range resolved {
		names[i] = rp.Name
	}
	return names, nil
}

// reviewerPassDirFor returns where a PR's pass artifacts live. The default is
// outside the worktree, so a PR's own files cannot land there and one PR's
// review does not overwrite another's.
func reviewerPassDirFor(pr int) (string, error) {
	if reviewerPassDir != "" {
		return reviewerPassDir, nil
	}
	rigPath := reviewerRigPathForHeartbeat()
	if rigPath == "" {
		return "", fmt.Errorf("cannot locate the rig; pass --dir")
	}
	return filepath.Join(rigPath, "reviewer", "passes", fmt.Sprintf("pr%d", pr)), nil
}

// verifiedPasses loads the receipted results for a PR at a commit.
func verifiedPasses(pr int, sha string) ([]reviewer.VerifiedPass, error) {
	names, err := reviewerEnabledPerspectives()
	if err != nil {
		return nil, err
	}
	dir, err := reviewerPassDirFor(pr)
	if err != nil {
		return nil, err
	}
	return reviewer.VerifyPasses(dir, names, pr, sha)
}

// ownDiff is a PR head's change against its base, without the parts that move
// when only the base does: blob ids and hunk headers' function context. Line
// numbers stay, because findings are anchored to them.
func ownDiff(base, head string) (string, error) {
	if !isHexObjectName(base) || !isHexObjectName(head) {
		return "", fmt.Errorf("diff base for %s unavailable", shortSHA(head))
	}
	out, err := exec.Command("git", "diff", "--no-ext-diff", "--no-textconv", "--no-color", //nolint:gosec // validated object names
		"--no-renames", "--binary", "--full-index", "--submodule=short", "--ignore-submodules=none",
		"-U0", base, head, "--").Output()
	if err != nil {
		return "", fmt.Errorf("diffing %s..%s: %w", shortSHA(base), shortSHA(head), err)
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(out), "\n") {
		if strings.HasPrefix(line, "index ") {
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			if end := strings.Index(line[3:], " @@"); end >= 0 {
				line = line[:3+end+3] + "\n"
			}
		}
		b.WriteString(line)
	}
	return b.String(), nil
}

// requireSameOwnDiff fails unless the PR changes exactly the same thing at both
// heads, as when the new head only merges the base branch.
func requireSameOwnDiff(pr int, from, to string) error {
	before, err := ownDiff(reviewerBaseSHA(pr, from), from)
	if err != nil {
		return err
	}
	after, err := ownDiff(reviewerBaseSHA(pr, to), to)
	if err != nil {
		return err
	}
	if before == "" || before != after {
		return fmt.Errorf("cannot carry the review from %s to %s: the PR's own diff changed (or is empty); "+
			"run the passes on the new head", shortSHA(from), shortSHA(to))
	}
	return nil
}

// consolidateVerifiedPasses builds the review payload from receipted results.
// carriedFrom names an earlier head the passes ran at.
func consolidateVerifiedPasses(pr int, sha, carriedFrom string, edits *reviewer.Edits) (*reviewer.Findings, error) {
	sha, carriedFrom = strings.TrimSpace(sha), strings.TrimSpace(carriedFrom)
	if sha == "" {
		return nil, fmt.Errorf("--sha is required: the pass receipts are checked against it")
	}
	ranAt := sha
	if carriedFrom != "" {
		ranAt = carriedFrom
	}
	passes, err := verifiedPasses(pr, ranAt)
	if err != nil {
		return nil, err
	}
	// The receipts hold the full object name, which the review anchor needs.
	full := passes[0].Receipt.SHA
	if carriedFrom != "" {
		out, err := exec.Command("git", "rev-parse", "--verify", "--end-of-options", sha+"^{commit}").Output() //nolint:gosec // end-of-options guards the value
		if err != nil {
			return nil, fmt.Errorf("resolving --sha %s: %w", sha, err)
		}
		carriedFrom, full = full, strings.TrimSpace(string(out))
		if err := requireSameOwnDiff(pr, carriedFrom, full); err != nil {
			return nil, err
		}
	}

	// A nil manifest leaves every finding unclassified, so an unavailable diff
	// never downgrades a review.
	base := edits.BaseSHA
	if base == "" {
		base = reviewerBaseSHA(pr, full)
	}
	fs, err := reviewer.ConsolidatePasses(passes, edits, full, buildDiffManifest(base, full))
	if err != nil {
		return nil, err
	}
	fs.CarriedFrom = carriedFrom
	return fs, nil
}

// gateReviewPost re-derives the payload from the receipted results and refuses
// a supplied payload that differs from it.
func gateReviewPost(pr int, shaFlag string, supplied *reviewer.Findings) (*reviewer.Findings, error) {
	sha := supplied.ReviewedSHA
	if sha == "" {
		return nil, fmt.Errorf("refusing to post: the payload has no reviewed_sha; build it with `gt reviewer consolidate`")
	}
	if shaFlag != "" && !reviewer.SameCommit(shaFlag, sha) {
		return nil, fmt.Errorf("refusing to post: --sha %s differs from the payload's reviewed_sha %s", shaFlag, sha)
	}
	ranAt := sha
	if supplied.CarriedFrom != "" {
		ranAt = supplied.CarriedFrom
	}
	passes, err := verifiedPasses(pr, ranAt)
	if err != nil {
		return nil, fmt.Errorf("refusing to post: %w", err)
	}
	if err := reviewer.UntracedFinding(passes, supplied.Findings); err != nil {
		return nil, fmt.Errorf("refusing to post: %w", err)
	}
	edits := supplied.Edits
	if edits == nil {
		edits = &reviewer.Edits{}
	}
	derived, err := consolidateVerifiedPasses(pr, sha, supplied.CarriedFrom, edits)
	if err != nil {
		return nil, fmt.Errorf("refusing to post: %w", err)
	}
	encoded, err := json.Marshal(derived)
	if err != nil {
		return nil, err
	}
	expected, err := reviewer.ParseFindings(encoded)
	if err != nil {
		return nil, fmt.Errorf("refusing to post: %w", err)
	}
	if !reflect.DeepEqual(expected, supplied) {
		return nil, fmt.Errorf("refusing to post: the payload is not what `gt reviewer consolidate` builds " +
			"from the verified pass results; re-run consolidate (use --edits, not a file editor)")
	}
	return expected, nil
}

func runReviewerRun(cmd *cobra.Command, args []string) error {
	if reviewerPromptPR <= 0 {
		return fmt.Errorf("--pr must be a positive PR number")
	}
	cwd, err := requireReviewerWorktree()
	if err != nil {
		return err
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("reading worktree HEAD: %w", err)
	}
	sha := strings.TrimSpace(string(head))
	if !reviewer.SameCommit(sha, reviewerPromptSHA) {
		return fmt.Errorf("worktree is at %s, not --sha %s: run `gt reviewer checkout` first",
			shortSHA(sha), reviewerPromptSHA)
	}

	townRoot, rigName, rigPath, reviewCfg, err := loadRigReviewConfig()
	if err != nil {
		return err
	}
	enabled, err := reviewerEnabledPerspectives()
	if err != nil {
		return err
	}
	names := args
	if len(names) == 0 {
		names = enabled
	}
	for i, name := range names {
		if !slices.Contains(enabled, name) || slices.Contains(names[:i], name) {
			return fmt.Errorf("perspective %q is not enabled, or is named twice (enabled: %s)",
				name, strings.Join(enabled, ", "))
		}
	}
	dir, err := reviewerPassDirFor(reviewerPromptPR)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // operator-facing artifacts
		return fmt.Errorf("creating pass directory: %w", err)
	}
	// Cleared before anything can fail, so an aborted run leaves nothing behind.
	if err := reviewer.RemovePassArtifacts(dir, names); err != nil {
		return err
	}

	priorThreads, err := readOptionalInput(reviewerPromptPriorThreads)
	if err != nil {
		return fmt.Errorf("reading --prior-threads: %w", err)
	}
	extra, err := readOptionalInput(reviewerPromptInstructions)
	if err != nil {
		return fmt.Errorf("reading --instructions: %w", err)
	}
	invoke, err := claudePassInvoker(townRoot, rigPath, cwd)
	if err != nil {
		return err
	}
	baseSHA := resolveReviewBaseSHA(reviewerPromptPR, sha)

	specs := make([]reviewer.PassSpec, len(names))
	for i, name := range names {
		prompt, err := buildReviewerPassPrompt(townRoot, rigName, rigPath, reviewCfg, name, baseSHA, priorThreads, extra)
		if err != nil {
			return err
		}
		specs[i] = reviewer.PassSpec{
			Perspective:       name,
			PR:                reviewerPromptPR,
			SHA:               sha,
			Round:             reviewerPromptRound,
			Prompt:            prompt,
			ExtraInstructions: strings.TrimSpace(extra) != "",
		}
	}
	touchReviewerPhase(reviewer.PhasePrompt)

	// The hard limit; the prompt's own budget is shorter.
	limit, _ := reviewer.ClampStuckThreshold(reviewer.StuckThreshold(townRoot, rigPath))
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	errs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := reviewer.RunPass(ctx, dir, spec, invoke)
			if err != nil {
				errs[i] = err
				fmt.Printf("✗ %s: %v\n", spec.Perspective, err)
				return
			}
			fmt.Printf("✓ %s: finished %s\n", spec.Perspective, receipt.FinishedAt.Format(time.RFC3339))
		}()
	}
	wg.Wait()
	fmt.Printf("Raw results (do not edit): %s\n", dir)
	return errors.Join(errs...)
}

// reviewerPassSettings reduces the reviewer role's settings to what a pass must
// inherit: the tool guards. The session hooks would prime the pass as the
// reviewer itself.
func reviewerPassSettings(roleSettings []byte) (string, error) {
	var in struct {
		Hooks          map[string]json.RawMessage `json:"hooks"`
		EnabledPlugins json.RawMessage            `json:"enabledPlugins,omitempty"`
	}
	if err := json.Unmarshal(roleSettings, &in); err != nil {
		return "", fmt.Errorf("parsing reviewer settings: %w", err)
	}
	guards, ok := in.Hooks["PreToolUse"]
	if !ok {
		return "", fmt.Errorf("reviewer settings have no PreToolUse guards")
	}
	in.Hooks = map[string]json.RawMessage{"PreToolUse": guards}
	out, err := json.Marshal(in)
	return string(out), err
}

// claudePassArgs builds the headless argv for a pass. The worktree holds the
// PR's own files, so project and local settings are not loaded (a PR could ship
// hooks in them), and only the codegraph MCP server is available.
func claudePassArgs(roleArgs []string, settings string) []string {
	var args []string
	for i := 0; i < len(roleArgs); i++ {
		if roleArgs[i] == "--settings" {
			i++
			continue
		}
		args = append(args, roleArgs[i])
	}
	return append(args,
		"--settings", settings,
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{"codegraph":{"type":"stdio","command":"codegraph","args":["serve","--mcp"]}}}`,
		"--output-format", "json",
		// The prompt arrives on stdin.
		"-p",
	)
}

// claudeAnswer returns the final answer from a `--output-format json` envelope,
// failing for any run that did not end successfully.
func claudeAnswer(stdout []byte) (string, error) {
	var env claudeResultEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return "", fmt.Errorf("unreadable agent output: %w: %s", err, truncateStr(string(stdout), 500))
	}
	if env.IsError || env.Subtype != "success" {
		return "", fmt.Errorf("agent ended with %q: %s", env.Subtype, truncateStr(env.Result, 500))
	}
	return env.Result, nil
}

// claudePassInvoker runs passes on the reviewer role's Claude configuration.
// Roles configured with another agent run their passes on the default Claude.
func claudePassInvoker(townRoot, rigPath, worktree string) (reviewer.PassInvoker, error) {
	settingsPath := filepath.Join(config.RoleSettingsDir("reviewer", rigPath), ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath) //nolint:gosec // fixed path under the rig
	if err != nil {
		return nil, fmt.Errorf("reading reviewer tool guards: %w", err)
	}
	settings, err := reviewerPassSettings(data)
	if err != nil {
		return nil, err
	}

	command, roleArgs := "claude", []string{"--dangerously-skip-permissions"}
	env := clearClaudeCodeEnv(os.Environ())
	if rc := config.ResolveRoleAgentConfig("reviewer", townRoot, rigPath); rc != nil &&
		strings.TrimSuffix(filepath.Base(rc.Command), ".exe") == "claude" {
		command, roleArgs = rc.Command, rc.Args
		for k, v := range rc.Env {
			env = append(env, k+"="+v)
		}
	}
	args := claudePassArgs(roleArgs, settings)

	return func(ctx context.Context, prompt string) (string, error) {
		c := exec.CommandContext(ctx, command, args...) //nolint:gosec // configured agent binary
		util.SetProcessGroup(c)
		c.WaitDelay = 10 * time.Second
		c.Dir = worktree
		c.Env = env
		c.Stdin = strings.NewReader(prompt)
		out, err := c.Output()
		if err != nil {
			return "", claudeCompactError(err, out)
		}
		return claudeAnswer(out)
	}, nil
}
