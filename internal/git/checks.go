package git

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
)

type checkState int

const (
	checkPassed checkState = iota
	checkPending
	checkFailed
)

// headCheck is one check run or commit status on a specific commit.
type headCheck struct {
	name  string
	appID int64 // 0 for commit statuses, which do not report an app
	state checkState
}

// requiredCheck is a check the base branch requires before merging.
type requiredCheck struct {
	context string
	appID   int64 // 0 or negative when any app may report it
}

// A commit status does not report which app sent it, so it never satisfies a
// requirement pinned to an app.

func (r requiredCheck) satisfiedBy(c headCheck) bool {
	return c.name == r.context && (r.appID <= 0 || c.appID == r.appID)
}

// GhPrChecksRollup returns the CI status of the PR's current head commit:
//   - state:  "SUCCESS", "FAILURE", "PENDING" or "NO_CHECKS"
//   - done:   true only for SUCCESS and FAILURE
//
// Checks are read for the head SHA itself: just after a push, the new head has
// no checks, or only some of them, so an absence of failing or pending checks
// proves nothing. SUCCESS needs at least one check, every check required by the
// base branch present, and every check finished. A head with no checks is
// NO_CHECKS with done=false, so a polling caller times out rather than passes.
func (g *Git) GhPrChecksRollup(prNumber int) (state string, done bool, err error) {
	head, base, err := g.ghPrHeadAndBase(prNumber)
	if err != nil {
		return "", false, err
	}
	owner, repo, err := g.ghRepoOwnerName()
	if err != nil {
		return "", false, err
	}
	repoPath := "repos/" + owner + "/" + repo

	checks, err := g.ghCommitChecks(repoPath, head)
	if err != nil {
		return "", false, err
	}
	if len(checks) == 0 {
		return "NO_CHECKS", false, nil
	}
	required, err := g.ghRequiredChecks(repoPath, base)
	if err != nil {
		return "", false, err
	}

	state = rollupChecks(checks, required)
	if state == "PENDING" {
		return state, false, nil
	}
	// A push during the reads above makes this verdict about a stale head.
	now, _, err := g.ghPrHeadAndBase(prNumber)
	if err != nil {
		return "", false, err
	}
	if now != head {
		return "PENDING", false, nil
	}
	return state, true, nil
}

func rollupChecks(checks []headCheck, required []requiredCheck) string {
	for _, r := range required {
		present := false
		for _, c := range checks {
			present = present || r.satisfiedBy(c)
		}
		if !present {
			return "PENDING"
		}
	}
	result := "SUCCESS"
	for _, c := range checks {
		switch c.state {
		case checkPending:
			return "PENDING"
		case checkFailed:
			result = "FAILURE"
		}
	}
	return result
}

func (g *Git) ghPrHeadAndBase(prNumber int) (head, base string, err error) {
	out, err := g.ghOutput("pr", "view", fmt.Sprintf("%d", prNumber), "--json", "headRefOid,baseRefName")
	if err != nil {
		return "", "", err
	}
	var resp struct {
		HeadRefOid  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return "", "", fmt.Errorf("parsing PR head and base: %w", err)
	}
	if resp.HeadRefOid == "" || resp.BaseRefName == "" {
		return "", "", fmt.Errorf("gh pr view returned no head or base for PR #%d", prNumber)
	}
	return resp.HeadRefOid, resp.BaseRefName, nil
}

// ghCommitChecks returns the check runs and commit statuses on sha.
func (g *Git) ghCommitChecks(repoPath, sha string) ([]headCheck, error) {
	var checks []headCheck

	runs, err := ghList[struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		App        struct {
			ID int64 `json:"id"`
		} `json:"app"`
	}](g, repoPath+"/commits/"+sha+"/check-runs?per_page=100", ".check_runs[]")
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		state := checkFailed
		switch {
		case r.Status != "completed":
			state = checkPending
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
			state = checkPassed
		}
		checks = append(checks, headCheck{name: r.Name, appID: r.App.ID, state: state})
	}

	statuses, err := ghList[struct {
		Context string `json:"context"`
		State   string `json:"state"`
	}](g, repoPath+"/commits/"+sha+"/status?per_page=100", ".statuses[]")
	if err != nil {
		return nil, err
	}
	for _, s := range statuses {
		state := checkFailed
		switch s.State {
		case "success":
			state = checkPassed
		case "pending":
			state = checkPending
		}
		checks = append(checks, headCheck{name: s.Context, state: state})
	}
	return checks, nil
}

// ghRequiredChecks returns the checks the branch requires, from both rulesets
// and classic branch protection. Any failure to read them is an error: an
// unknown requirement must not be treated as no requirement.
func (g *Git) ghRequiredChecks(repoPath, branch string) ([]requiredCheck, error) {
	branch = url.PathEscape(branch)
	var required []requiredCheck

	rules, err := ghList[struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context       string `json:"context"`
				IntegrationID int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}](g, repoPath+"/rules/branches/"+branch+"?per_page=100", ".[]")
	// A plan without rulesets answers 403 "Upgrade to GitHub ...", which does
	// mean there are none.
	if err != nil && !strings.Contains(err.Error(), "Upgrade to GitHub") {
		return nil, err
	}
	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, c := range rule.Parameters.Checks {
			required = append(required, requiredCheck{context: c.Context, appID: c.IntegrationID})
		}
	}

	out, err := g.ghOutput("api", repoPath+"/branches/"+branch)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Protection struct {
			Required struct {
				EnforcementLevel string   `json:"enforcement_level"`
				Contexts         []string `json:"contexts"`
				Checks           []struct {
					Context string `json:"context"`
					AppID   int64  `json:"app_id"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, fmt.Errorf("parsing branch protection: %w", err)
	}
	classic := resp.Protection.Required
	if classic.EnforcementLevel == "off" {
		return required, nil
	}
	for _, c := range classic.Checks {
		required = append(required, requiredCheck{context: c.Context, appID: c.AppID})
	}
	// contexts repeats checks by name; it is the only list on older servers.
	for _, name := range classic.Contexts {
		required = append(required, requiredCheck{context: name})
	}
	return required, nil
}

// ghList reads every page of a GitHub list endpoint, selecting items with jq.
func ghList[T any](g *Git, path, jq string) ([]T, error) {
	out, err := g.ghOutput("api", "--paginate", path, "--jq", jq)
	if err != nil {
		return nil, err
	}
	var items []T
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var item T
		if err := dec.Decode(&item); errors.Is(err, io.EOF) {
			return items, nil
		} else if err != nil {
			return nil, fmt.Errorf("parsing gh api %s: %w", path, err)
		}
		items = append(items, item)
	}
}

// ghOutput runs gh in the work dir and returns stdout, with stderr in the error.
func (g *Git) ghOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %s failed: %s: %w", strings.Join(args, " "),
			strings.TrimSpace(stderr.String()), err)
	}
	return stdout.Bytes(), nil
}
