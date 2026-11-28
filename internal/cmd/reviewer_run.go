package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hooks"
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
<rig>/reviewer/passes/pr<N>/, outside the worktree. Both are written once every
pass has exited, and each result's sha256 is printed. 'consolidate' does not
check the receipts.

With no arguments every enabled perspective runs; name one to re-run it. A pass
can take longer than a foreground shell timeout, so run this in the background
and wait for it to exit.`,
	RunE: runReviewerRun,
}

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

func runReviewerRun(cmd *cobra.Command, args []string) error {
	if reviewerPromptPR <= 0 {
		return fmt.Errorf("--pr must be a positive PR number")
	}
	if reviewerPromptPriorThreads == "-" && reviewerPromptInstructions == "-" {
		return fmt.Errorf("--prior-threads and --instructions cannot both read from stdin (\"-\")")
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

	err = runReviewerPasses(ctx, os.Stdout, dir, specs, invoke)
	fmt.Printf("Raw results (do not edit): %s\n", dir)
	return err
}

// runReviewerPasses runs the passes in parallel and writes their results only
// after all of them have exited: a running pass has shell access to dir and
// could otherwise replace a sibling's finished result and receipt. Each result's
// sha256 goes to out, which no pass can rewrite.
func runReviewerPasses(ctx context.Context, out io.Writer, dir string, specs []reviewer.PassSpec,
	invoke reviewer.PassInvoker) error {
	passes := make([]*reviewer.CompletedPass, len(specs))
	errs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			passes[i], errs[i] = reviewer.RunPass(ctx, dir, spec, invoke)
		}()
	}
	wg.Wait()

	for i, spec := range specs {
		if errs[i] != nil {
			// A sibling may have planted files where a failed pass wrote none.
			errs[i] = errors.Join(errs[i], reviewer.RemovePassArtifacts(dir, []string{spec.Perspective}))
		} else {
			errs[i] = passes[i].Write(dir)
		}
		if errs[i] != nil {
			_, _ = fmt.Fprintf(out, "✗ %s: %v\n", spec.Perspective, errs[i])
			continue
		}
		receipt := passes[i].Receipt
		_, _ = fmt.Fprintf(out, "✓ %s: finished %s, result sha256 %s\n",
			spec.Perspective, receipt.FinishedAt.Format(time.RFC3339), receipt.ResultSHA256)
	}
	return errors.Join(errs...)
}

// reviewerPassSettings renders what a pass inherits from the reviewer role: the
// tool guards, built from the role's hook configuration as 'gt hooks sync' builds
// them. No settings file is read from the rig, whose worktree holds the PR's own
// files. Session hooks are left out: they would prime the pass as the reviewer
// itself.
func reviewerPassSettings(rigName string) ([]byte, error) {
	cfg, err := hooks.ComputeExpected(rigName + "/reviewer")
	if err != nil {
		return nil, fmt.Errorf("building reviewer tool guards: %w", err)
	}
	if len(cfg.PreToolUse) == 0 {
		return nil, fmt.Errorf("reviewer role has no PreToolUse guards")
	}
	return json.Marshal(map[string]any{
		"hooks":          hooks.HooksConfig{PreToolUse: cfg.PreToolUse},
		"enabledPlugins": map[string]bool{"beads@beads-marketplace": false},
	})
}

// stripPassConfigArgs drops every argument that chooses settings or MCP servers,
// in both the "--flag value" and "--flag=value" forms, so the runner's own are
// the only ones. Everything from "--" on goes too: flags appended after it would
// be read as positional arguments.
func stripPassConfigArgs(roleArgs []string) []string {
	var args []string
	for i := 0; i < len(roleArgs); i++ {
		if roleArgs[i] == "--" {
			break
		}
		flag, _, inline := strings.Cut(roleArgs[i], "=")
		switch flag {
		case "--settings", "--setting-sources":
			if !inline {
				i++
			}
		case "--mcp-config":
			// Takes several values.
			for i+1 < len(roleArgs) && !strings.HasPrefix(roleArgs[i+1], "-") {
				i++
			}
		case "--strict-mcp-config":
		default:
			args = append(args, roleArgs[i])
		}
	}
	return args
}

// claudePassArgs builds the headless argv for a pass. The worktree holds the
// PR's own files, so project and local settings are not loaded (a PR could ship
// hooks in them), and only the codegraph MCP server is available.
func claudePassArgs(roleArgs []string, settingsPath string) []string {
	return append(stripPassConfigArgs(roleArgs),
		"--settings", settingsPath,
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
	settings, err := reviewerPassSettings(filepath.Base(rigPath))
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

	return func(ctx context.Context, prompt string) (string, error) {
		// A fresh file per launch, so no earlier pass has had its path to edit.
		settingsPath, err := writeTempSettings(settings)
		if err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(settingsPath) }()

		args := claudePassArgs(roleArgs, settingsPath)
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

// writeTempSettings writes the pass guards to a private file outside the
// worktree and returns its path.
func writeTempSettings(settings []byte) (string, error) {
	f, err := os.CreateTemp("", "gt-pass-settings-*.json")
	if err != nil {
		return "", fmt.Errorf("creating pass settings file: %w", err)
	}
	_, werr := f.Write(settings)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("writing pass settings file: %w", err)
	}
	return f.Name(), nil
}
