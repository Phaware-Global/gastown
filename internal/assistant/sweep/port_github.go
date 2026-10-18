package sweep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/telegraph"
)

// GitHub port defaults (design: docs/design/assistant-role.md, "GitHub port").
const (
	githubDefaultTokenEnv = "GITHUB_TOKEN" //nolint:gosec // G101: the name of an environment variable, not a secret
	githubPRPage          = 50
	githubMaxPRPages      = 3 // open PRs per repo; more is reported as partial
)

// repoRef is one repository the port sweeps, with the rig settings that
// decide who the internal reviewer and the required approver are.
type repoRef struct {
	Owner, Name string
	Rig         string // "" for extra repos
	Reviewer    string // merge_queue.pr_reviewer login
	Approver    string // merge_queue.pr_approver login
}

func (r repoRef) slug() string { return r.Owner + "/" + r.Name }

// GitHubPort reports PRs waiting on humans, stale approvals, unanswered
// review feedback, missing internal review loops, failing CI nobody owns and
// abandoned drafts. It only reads. (release-unclosed follows in the next
// layer together with the PR-linked Jira kinds.)
type GitHubPort struct {
	townRoot string
	persona  func() ([]string, []string, error) // logins, telegraph extra repos
	repos    func() ([]repoRef, error)
	token    func(env string) string
	// patrolOwns reports whether a non-closed pr-feedback-patrol bead for the
	// PR exists in the rig (then a polecat already owns the CI failure).
	patrolOwns func(rig string, pr int) (bool, error)
	client     *http.Client
	apiBase    string // "https://api.github.com"
}

// NewGitHubPort returns the production GitHub port.
func NewGitHubPort(townRoot string, getenv func(string) string) *GitHubPort {
	return &GitHubPort{
		townRoot: townRoot,
		persona: func() ([]string, []string, error) {
			cfg, err := telegraph.LoadWithDefaults(telegraph.DefaultPath(townRoot))
			if err != nil {
				return nil, nil, err
			}
			var extra []string
			if gh := cfg.Telegraph.Providers["github"]; gh != nil {
				extra = gh.ExtraRepos
			}
			return cfg.Telegraph.Mayor.GitHubLogins, extra, nil
		},
		repos: func() ([]repoRef, error) { return rigRepos(townRoot) },
		token: func(env string) string {
			if t := getenv(env); t != "" {
				return t
			}
			out, err := exec.Command("gh", "auth", "token").Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		},
		patrolOwns: func(rig string, pr int) (bool, error) {
			issues, err := beads.New(filepath.Join(townRoot, rig)).List(beads.ListOptions{Status: "all", Label: "pr-feedback-patrol", NoWisps: true})
			if err != nil {
				return false, err
			}
			want := fmt.Sprintf("pr-num-%d", pr)
			for _, i := range issues {
				if i.Status == "closed" {
					continue
				}
				for _, l := range i.Labels {
					if l == want {
						return true, nil
					}
				}
			}
			return false, nil
		},
		client:  &http.Client{Timeout: 30 * time.Second},
		apiBase: "https://api.github.com",
	}
}

// rigRepos reads the rig registry and each rig's merge-queue settings.
func rigRepos(townRoot string) ([]repoRef, error) {
	cfg, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, err
	}
	var out []repoRef
	for name, entry := range cfg.Rigs {
		owner, repo, ok := githubOwnerRepo(entry.GitURL)
		if !ok {
			continue
		}
		ref := repoRef{Owner: owner, Name: repo, Rig: name}
		if rs, err := config.LoadRigSettings(config.RigSettingsPath(filepath.Join(townRoot, name))); err == nil && rs != nil && rs.MergeQueue != nil {
			ref.Reviewer, ref.Approver = rs.MergeQueue.PRReviewer, rs.MergeQueue.PRApprover
		}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].slug() < out[j].slug() })
	return out, nil
}

// githubOwnerRepo parses https://github.com/o/r(.git) and git@github.com:o/r(.git).
func githubOwnerRepo(raw string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		if strings.HasPrefix(raw, prefix) {
			parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(raw, prefix), ".git"), "/")
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				return parts[0], parts[1], true
			}
		}
	}
	return "", "", false
}

func (p *GitHubPort) Name() string { return "github" }

func (p *GitHubPort) Suggestions() []string {
	return []string{
		"close or finish {key}",
		"request review from {approver} on {key}",
		"request human review on {key}",
		"re-ping {human} on {key}",
		"ask {human} to re-approve {key}",
		"check why {key} is not merged yet",
		"respond to review comments on {key}",
		"start the internal review loop on {key}",
		"fix ci on {key}",
		"add them to assistant.sweep.github.humans or ignore",
	}
}

func (p *GitHubPort) Schema() map[string]Shape {
	return map[string]Shape{
		"human":        ShapeGitHubLogin, // listed human we wait on
		"approver":     ShapeGitHubLogin, // rig's configured approver
		"reviewer":     ShapeGitHubLogin, // rig's configured internal reviewer
		"head":         ShapeSHA,
		"ci":           ShapeEnum, // success | failure | pending | none
		"threads":      ShapeInt,  // unresolved review threads
		"requested_at": ShapeTime,
		"rig":          ShapeEnum,
	}
}

// --- GraphQL wire types ----------------------------------------------------

type ghActor struct {
	Login string `json:"login"`
}

type ghReview struct {
	Author      ghActor   `json:"author"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
	Commit      struct {
		Oid string `json:"oid"`
	} `json:"commit"`
}

type ghThreadComment struct {
	Author    ghActor   `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
}

type ghThread struct {
	IsResolved bool `json:"isResolved"`
	Comments   struct {
		Nodes []ghThreadComment `json:"nodes"`
	} `json:"comments"`
}

type ghCommit struct {
	PushedDate        *time.Time `json:"pushedDate"`
	CommittedDate     time.Time  `json:"committedDate"`
	StatusCheckRollup *struct {
		State string `json:"state"`
	} `json:"statusCheckRollup"`
}

type ghRequestEvent struct {
	CreatedAt         time.Time `json:"createdAt"`
	RequestedReviewer ghActor   `json:"requestedReviewer"`
}

type ghPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	CreatedAt      time.Time `json:"createdAt"`
	HeadRefName    string    `json:"headRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	BaseRefName    string    `json:"baseRefName"`
	Author         ghActor   `json:"author"`
	ReviewRequests struct {
		Nodes []ghRequestEvent `json:"nodes"` // only RequestedReviewer is populated
	} `json:"reviewRequests"`
	Reviews struct {
		Nodes []ghReview `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []ghThread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit ghCommit `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	TimelineItems struct {
		Nodes []ghRequestEvent `json:"nodes"`
	} `json:"timelineItems"`
}

type ghRepoData struct {
	Open struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []ghPR `json:"nodes"`
	} `json:"open"`
}

const githubRepoQuery = `query($owner:String!,$name:String!,$after:String){
  repository(owner:$owner,name:$name){
    open: pullRequests(states:OPEN, first:50, after:$after, orderBy:{field:UPDATED_AT,direction:DESC}){
      pageInfo{hasNextPage endCursor}
      nodes{
        number title url isDraft createdAt headRefName headRefOid baseRefName author{login}
        reviewRequests(first:20){nodes{requestedReviewer{... on User{login}}}}
        reviews(last:50){nodes{author{login} state submittedAt commit{oid}}}
        reviewThreads(first:50){nodes{isResolved comments(first:1){nodes{author{login} createdAt}}}}
        commits(last:1){nodes{commit{pushedDate committedDate statusCheckRollup{state}}}}
        timelineItems(last:30, itemTypes:[REVIEW_REQUESTED_EVENT]){nodes{... on ReviewRequestedEvent{createdAt requestedReviewer{... on User{login}}}}}
      }
    }
  }
}`

type ghClient struct {
	ctx   context.Context
	port  *GitHubPort
	token string
	trace func(string)
	dry   bool
}

func (c *ghClient) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.port.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, truncate(string(body), 300))
	}
	return json.Unmarshal(body, out)
}

func (c *ghClient) repo(ref repoRef, after string) (*ghRepoData, error) {
	if c.trace != nil {
		c.trace(fmt.Sprintf("POST /graphql repository(%s) open PRs after=%q", ref.slug(), after))
	}
	if c.dry {
		return &ghRepoData{}, nil
	}
	vars := map[string]any{"owner": ref.Owner, "name": ref.Name}
	if after != "" {
		vars["after"] = after
	}
	body, _ := json.Marshal(map[string]any{"query": githubRepoQuery, "variables": vars})
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.port.apiBase+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Repository *ghRepoData `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(req, &res); err != nil {
		return nil, err
	}
	if res.Data.Repository == nil {
		msg := "repository not found or not accessible"
		if len(res.Errors) > 0 {
			msg = res.Errors[0].Message
		}
		return nil, fmt.Errorf("%s: %s", ref.slug(), msg)
	}
	return res.Data.Repository, nil
}

// --- the sweep -------------------------------------------------------------

type githubDurations struct {
	remind, approvedUnmerged, respond, reviewLoop, ciFailing, stale time.Duration
}

func (p *GitHubPort) Sweep(ctx context.Context, in Input) ([]Finding, error) {
	cfg := in.Settings.GitHubSweep()
	logins, tgExtra, err := p.persona()
	if err != nil {
		return nil, fmt.Errorf("reading telegraph.toml: %w", err)
	}
	isPersona := set(logins)
	if len(isPersona) == 0 {
		return nil, fmt.Errorf("no persona login configured (telegraph.mayor.github_logins)")
	}
	tokenEnv := cfg.TokenEnv
	if tokenEnv == "" {
		tokenEnv = githubDefaultTokenEnv
	}
	c := &ghClient{ctx: ctx, port: p, trace: in.Trace, dry: in.DryRun}
	if !in.DryRun {
		if c.token = p.token(tokenEnv); c.token == "" {
			return nil, fmt.Errorf("github credentials missing: set %s or authenticate gh", tokenEnv)
		}
	}
	repos, err := p.repos()
	if err != nil {
		return nil, fmt.Errorf("reading rig registry: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range repos {
		seen[strings.ToLower(r.slug())] = true
	}
	for _, extra := range append(append([]string(nil), tgExtra...), cfg.ExtraRepos...) {
		parts := strings.Split(strings.TrimSpace(extra), "/")
		if len(parts) != 2 || seen[strings.ToLower(extra)] {
			continue
		}
		seen[strings.ToLower(extra)] = true
		repos = append(repos, repoRef{Owner: parts[0], Name: parts[1]})
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no GitHub repositories: rig registry has none and extra_repos is empty")
	}
	d := githubDurations{
		remind:           parseDur(cfg.RemindAfter, 24*time.Hour),
		approvedUnmerged: parseDur(cfg.ApprovedUnmergedAfter, 4*time.Hour),
		respond:          parseDur(cfg.RespondAfter, 12*time.Hour),
		reviewLoop:       parseDur(cfg.ReviewLoopAfter, 2*time.Hour),
		ciFailing:        parseDur(cfg.CIFailingAfter, time.Hour),
		stale:            parseDur(cfg.StaleAfter, 7*24*time.Hour),
	}
	isHuman := set(cfg.Humans)
	var out []Finding
	var partial []string
	note := func(format string, args ...any) { partial = append(partial, fmt.Sprintf(format, args...)) }

	for _, ref := range repos {
		var open []ghPR
		checked := false
		after := ""
		for page := 0; ; page++ {
			if page == githubMaxPRPages {
				note("%s: more than %d open PRs; the rest were not checked", ref.slug(), githubMaxPRPages*githubPRPage)
				break
			}
			d, err := c.repo(ref, after)
			if err != nil {
				note("%s: %v", ref.slug(), err)
				break
			}
			checked = true
			open = append(open, d.Open.Nodes...)
			if !d.Open.PageInfo.HasNextPage {
				break
			}
			after = d.Open.PageInfo.EndCursor
		}
		if !checked {
			continue
		}
		for i := range open {
			out = append(out, p.checkPR(ref, &open[i], in.Now, d, isPersona, isHuman, note)...)
		}
	}
	if len(partial) > 0 {
		return out, &PartialError{Reason: strings.Join(partial, "; ")}
	}
	return out, nil
}

func (p *GitHubPort) checkPR(ref repoRef, pr *ghPR, now time.Time, d githubDurations, isPersona, isHuman map[string]bool, note func(string, ...any)) []Finding {
	key := fmt.Sprintf("%s#%d", ref.slug(), pr.Number)
	ours := isPersona[pr.Author.Login]
	head := pr.HeadRefOid
	ci := "none"
	pushed := pr.CreatedAt
	if len(pr.Commits.Nodes) > 0 {
		cm := pr.Commits.Nodes[0].Commit
		if cm.PushedDate != nil {
			pushed = *cm.PushedDate
		} else if !cm.CommittedDate.IsZero() {
			pushed = cm.CommittedDate
		}
		if cm.StatusCheckRollup != nil {
			switch cm.StatusCheckRollup.State {
			case "SUCCESS":
				ci = "success"
			case "FAILURE", "ERROR":
				ci = "failure"
			default:
				ci = "pending"
			}
		}
	}
	base := func(kind, severity string) Finding {
		f := Finding{Kind: kind, Key: key, URL: pr.URL, Severity: severity, Since: pushed,
			Fields:   map[string]string{"head": head, "ci": ci},
			External: map[string]string{"title": pr.Title, "branch": pr.HeadRefName, "base": pr.BaseRefName, "author": pr.Author.Login}}
		if ref.Rig != "" {
			f.Fields["rig"] = ref.Rig
		}
		return f
	}
	// Latest review per login, and approvals on the current head.
	type rv struct {
		state string
		oid   string
		at    time.Time
	}
	latest := map[string]rv{}
	reviewerSawHead, reviewerEver := false, false
	for _, r := range pr.Reviews.Nodes {
		if r.State == "COMMENTED" || r.State == "PENDING" {
			continue
		}
		if ref.Reviewer != "" && strings.EqualFold(r.Author.Login, ref.Reviewer) {
			reviewerEver = true
			if r.Commit.Oid == head && r.State == "APPROVED" {
				reviewerSawHead = true
			}
		}
		if r.State == "DISMISSED" {
			delete(latest, r.Author.Login)
			continue
		}
		latest[r.Author.Login] = rv{r.State, r.Commit.Oid, r.SubmittedAt}
	}
	requested := map[string]time.Time{}
	for _, rr := range pr.ReviewRequests.Nodes {
		if l := rr.RequestedReviewer.Login; isHuman[l] {
			requested[l] = pr.CreatedAt
		}
	}
	for _, ev := range pr.TimelineItems.Nodes {
		if l := ev.RequestedReviewer.Login; requested[l].Before(ev.CreatedAt) {
			if _, ok := requested[l]; ok {
				requested[l] = ev.CreatedAt
			}
		}
	}
	var humanApprovedHead, humanApprovedOld, humanChanges string
	var approvedAt, changesAt time.Time
	for login, r := range latest {
		if !isHuman[login] {
			continue
		}
		switch {
		case r.state == "APPROVED" && r.oid == head:
			if humanApprovedHead == "" || (ref.Approver != "" && strings.EqualFold(login, ref.Approver)) {
				humanApprovedHead, approvedAt = login, r.at
			}
		case r.state == "APPROVED":
			humanApprovedOld = login
		case r.state == "CHANGES_REQUESTED":
			humanChanges, changesAt = login, r.at
		}
	}
	unresolved := 0
	var oldestThread time.Time
	for _, th := range pr.ReviewThreads.Nodes {
		if th.IsResolved || len(th.Comments.Nodes) == 0 || !isHuman[th.Comments.Nodes[0].Author.Login] {
			continue
		}
		unresolved++
		if at := th.Comments.Nodes[0].CreatedAt; oldestThread.IsZero() || at.Before(oldestThread) {
			oldestThread = at
		}
	}
	approverOK := humanApprovedHead != "" && (ref.Approver == "" || strings.EqualFold(humanApprovedHead, ref.Approver))

	var out []Finding
	// A reviewer or requested reviewer that is neither the persona, a listed
	// human nor the rig's internal reviewer is a config gap, not a human.
	known := func(l string) bool {
		return l == "" || isPersona[l] || isHuman[l] || strings.EqualFold(l, ref.Reviewer)
	}
	var strangers []string
	for _, rr := range pr.ReviewRequests.Nodes {
		if l := rr.RequestedReviewer.Login; !known(l) {
			strangers = append(strangers, l)
		}
	}
	for login := range latest {
		if !known(login) {
			strangers = append(strangers, login)
		}
	}
	if len(strangers) > 0 {
		sort.Strings(strangers)
		f := base("unknown-actor", SeverityInfo)
		f.External["logins"] = strings.Join(strangers, ", ")
		f.Suggest = "add them to assistant.sweep.github.humans or ignore"
		f.Fingerprint = strings.Join(strangers, ",")
		out = append(out, f)
	}
	if pr.IsDraft {
		if ours && now.Sub(pushed) >= d.stale {
			f := base("draft-stale", SeverityInfo)
			f.Suggest = "close or finish " + key
			f.Fingerprint = head
			out = append(out, f)
		}
		return out // nothing else applies to drafts
	}
	if ours && ref.Reviewer != "" && reviewerSawHead && ci == "success" && len(requested) == 0 && humanApprovedHead == "" && humanChanges == "" {
		f := base("ready-for-human-review", SeverityActionable)
		f.Fields["reviewer"] = ref.Reviewer
		if ref.Approver != "" {
			f.Fields["approver"] = ref.Approver
			f.Suggest = "request review from " + ref.Approver + " on " + key
		} else {
			f.Suggest = "request human review on " + key
		}
		f.Fingerprint = head
		out = append(out, f)
	}
	for login, at := range requested {
		if r, ok := latest[login]; ok && r.oid == head {
			continue // they reviewed this head
		}
		if age := now.Sub(at); age >= d.remind {
			// Keyed per requested human; the fingerprint counts remind_after
			// periods since the request and ignores the head, so a new push
			// on an unchanged request never re-pings on its own.
			f := base("review-waiting", SeverityActionable)
			f.Key = key + "@" + login
			f.Since = at
			f.Fields["human"] = login
			f.Fields["requested_at"] = at.UTC().Format(time.RFC3339)
			f.Suggest = "re-ping " + login + " on " + f.Key
			f.Fingerprint = fmt.Sprintf("%s|%d", login, int(age/d.remind)) // once per remind_after
			out = append(out, f)
		}
	}
	if humanApprovedHead == "" && humanApprovedOld != "" {
		f := base("approval-stale", SeverityActionable)
		f.Fields["human"] = humanApprovedOld
		f.Suggest = "ask " + humanApprovedOld + " to re-approve " + key
		f.Fingerprint = head
		out = append(out, f)
	}
	if approverOK && ci == "success" && now.Sub(approvedAt) >= d.approvedUnmerged {
		f := base("approved-unmerged", SeverityActionable)
		f.Since = approvedAt
		f.Fields["human"] = humanApprovedHead
		f.Suggest = "check why " + key + " is not merged yet"
		f.Fingerprint = head + "|" + approvedAt.UTC().Format(time.RFC3339)
		out = append(out, f)
	}
	if ours {
		var since time.Time
		if humanChanges != "" && changesAt.After(pushed) {
			since = changesAt
		}
		if unresolved > 0 && oldestThread.After(pushed) && (since.IsZero() || oldestThread.Before(since)) {
			since = oldestThread
		}
		if !since.IsZero() && now.Sub(since) >= d.respond {
			f := base("changes-requested-stale", SeverityActionable)
			f.Since = since
			f.Fields["threads"] = fmt.Sprint(unresolved)
			if humanChanges != "" {
				f.Fields["human"] = humanChanges
			}
			f.Suggest = "respond to review comments on " + key
			f.Fingerprint = fmt.Sprintf("%s|%d|%s", head, unresolved, humanChanges)
			out = append(out, f)
		}
		if ref.Reviewer != "" && !reviewerEver && now.Sub(pr.CreatedAt) >= d.reviewLoop {
			f := base("no-review-loop", SeverityActionable)
			f.Since = pr.CreatedAt
			f.Fields["reviewer"] = ref.Reviewer
			f.Suggest = "start the internal review loop on " + key
			f.Fingerprint = head
			out = append(out, f)
		}
	}
	if ci == "failure" && now.Sub(pushed) >= d.ciFailing {
		owned := false
		if ref.Rig != "" {
			var err error
			if owned, err = p.patrolOwns(ref.Rig, pr.Number); err != nil {
				note("%s: patrol beads unavailable (ci-failing reported without dedup): %v", key, err)
			}
		}
		if !owned {
			f := base("ci-failing", SeverityActionable)
			f.Suggest = "fix ci on " + key
			f.Fingerprint = head
			out = append(out, f)
		}
	}
	return out
}
