package git

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// OpenPR is the subset of an open GitHub pull request needed to decide
// whether a branch's commits already belong to a PR.
type OpenPR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
}

// ListOpenPRs returns the open pull requests of the repository via the gh CLI.
func (g *Git) ListOpenPRs() ([]OpenPR, error) {
	cmd := exec.Command("gh", "pr", "list", "--state", "open",
		"--json", "number,headRefName,baseRefName,isDraft", "--limit", "500")
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list failed: %w", err)
	}
	var prs []OpenPR
	if err := json.Unmarshal(bytes.TrimSpace(out), &prs); err != nil {
		return nil, fmt.Errorf("parsing gh pr list output: %w", err)
	}
	return prs, nil
}

// CommitsInRange returns the SHAs reachable from head but not from base.
func (g *Git) CommitsInRange(base, head string) ([]string, error) {
	out, err := g.run("rev-list", base+".."+head)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}
