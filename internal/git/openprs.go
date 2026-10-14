package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ghWaitDelay bounds how long a gh call may keep running after its context
// expires, so a grandchild holding the stdout pipe cannot defeat the deadline.
const ghWaitDelay = 5 * time.Second

// OpenPR is the subset of an open GitHub pull request needed to decide
// whether a branch's commits already belong to a PR.
type OpenPR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
	// IsCrossRepository is true for a PR whose head lives in a fork. Its
	// HeadRefName is the fork's branch name and says nothing about a branch
	// in this repository.
	IsCrossRepository bool `json:"isCrossRepository"`
}

// ListOpenPRs returns the open pull requests of the repository via the gh CLI.
// The call is bounded by remoteQueryTimeout so a stalled GitHub request fails
// instead of hanging the caller.
func (g *Git) ListOpenPRs() ([]OpenPR, error) {
	return g.listOpenPRs(remoteQueryTimeout)
}

func (g *Git) listOpenPRs(timeout time.Duration) ([]OpenPR, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "open",
		"--json", "number,headRefName,baseRefName,isDraft,isCrossRepository", "--limit", "500")
	cmd.Dir = g.workDir
	cmd.WaitDelay = ghWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("gh pr list timed out after %v", timeout)
		}
		return nil, fmt.Errorf("gh pr list failed: %w", err)
	}
	var prs []OpenPR
	if err := json.Unmarshal(bytes.TrimSpace(out), &prs); err != nil {
		return nil, fmt.Errorf("parsing gh pr list output: %w", err)
	}
	return prs, nil
}

// PatchIDsInRange returns the stable patch-id of every non-merge commit
// reachable from head but not from base. Patch-ids identify a change by its
// content, so they match across a rebase or cherry-pick that rewrites SHAs.
func (g *Git) PatchIDsInRange(base, head string) ([]string, error) {
	patches, err := g.run("log", "-p", "--no-merges", "--format=commit %H", base+".."+head)
	if err != nil {
		return nil, err
	}
	if patches == "" {
		return nil, nil
	}
	args := []string{"patch-id", "--stable"}
	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = g.workDir
	cmd.Stdin = strings.NewReader(patches + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git patch-id: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var ids []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			ids = append(ids, f[0])
		}
	}
	return ids, nil
}
