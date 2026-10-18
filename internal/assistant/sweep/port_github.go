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
	// patrolBeads returns the PR numbers that have a non-closed
	// pr-feedback-patrol bead in the rig (a polecat already owns that CI
	// failure). Called once per rig per sweep.
	patrolBeads func(rig string) (map[int]bool, error)
	client      *http.Client
	apiBase     string // "https://api.github.com"
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
		patrolBeads: func(rig string) (map[int]bool, error) {
			issues, err := beads.New(filepath.Join(townRoot, rig)).List(beads.ListOptions{Status: "all", Label: "pr-feedback-patrol", NoWisps: true})
			if err != nil {
				return nil, err
			}
			owned := map[int]bool{}
			for _, i := range issues {
				if i.Status == "closed" {
					continue
				}
				for _, l := range i.Labels {
					var n int
					if _, err := fmt.Sscanf(l, "pr-num-%d", &n); err == nil && n > 0 {
						owned[n] = true
					}
				}
			}
			return owned, nil
		},
		client:  &http.Client{Timeout: 30 * time.Second},
		apiBase: "https://api.github.com",
	}
}

// lowerSet is set() with case-folded members, for GitHub logins.
func lowerSet(items []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range items {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			m[s] = true
		}
	}
	return m
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

// ghPage flags a fixed-window connection that did not fit: the PR's review
// state is then not fully known and the port reports partial.
type ghPage struct {
	HasNextPage     bool `json:"hasNextPage"`
	HasPreviousPage bool `json:"hasPreviousPage"`
}

func (p ghPage) truncated() bool { return p.HasNextPage || p.HasPreviousPage }

type ghCommit struct {
	// pushedDate is deprecated and always null on GitHub; the push time is
	// the head's newest check suite, else a force-push event, else the
	// committer date.
	CommittedDate     time.Time `json:"committedDate"`
	StatusCheckRollup *struct {
		State string `json:"state"`
	} `json:"statusCheckRollup"`
	CheckSuites struct {
		Nodes []struct {
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"checkSuites"`
}

// ghRequestEvent is a ReviewRequestedEvent or a HeadRefForcePushedEvent.
type ghRequestEvent struct {
	Typename          string    `json:"__typename"`
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
		PageInfo ghPage           `json:"pageInfo"`
		Nodes    []ghRequestEvent `json:"nodes"` // only RequestedReviewer is populated
	} `json:"reviewRequests"`
	Reviews struct {
		PageInfo ghPage     `json:"pageInfo"`
		Nodes    []ghReview `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		PageInfo ghPage     `json:"pageInfo"`
		Nodes    []ghThread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit ghCommit `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	TimelineItems struct {
		PageInfo ghPage           `json:"pageInfo"`
		Nodes    []ghRequestEvent `json:"nodes"`
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
        reviewRequests(first:20){pageInfo{hasNextPage} nodes{requestedReviewer{... on User{login}}}}
        reviews(last:50){pageInfo{hasPreviousPage} nodes{author{login} state submittedAt commit{oid}}}
        reviewThreads(first:50){pageInfo{hasNextPage} nodes{isResolved comments(first:1){nodes{author{login} createdAt}}}}
        commits(last:1){nodes{commit{committedDate statusCheckRollup{state} checkSuites(last:1){nodes{createdAt}}}}}
        timelineItems(last:30, itemTypes:[REVIEW_REQUESTED_EVENT, HEAD_REF_FORCE_PUSHED_EVENT]){pageInfo{hasPreviousPage} nodes{__typename ... on ReviewRequestedEvent{createdAt requestedReviewer{... on User{login}}} ... on HeadRefForcePushedEvent{createdAt}}}
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
	ctx, cancel := withBudget(ctx)
	defer cancel()
	isPersona := lowerSet(logins)
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
	isHuman := lowerSet(cfg.Humans)
	for l := range isPersona {
		delete(isHuman, l) // the persona is never a human reviewer, even if listed
	}
	var out []Finding
	var partial []string
	note := func(format string, args ...any) { partial = append(partial, fmt.Sprintf(format, args...)) }
	// One patrol-bead lookup per rig per sweep.
	patrol := map[string]map[int]bool{}
	owns := func(rig string, n int) (bool, error) {
		set, ok := patrol[rig]
		if !ok {
			var err error
			if set, err = p.patrolBeads(rig); err != nil {
				return false, err
			}
			if set == nil {
				set = map[int]bool{}
			}
			patrol[rig] = set
		}
		return set[n], nil
	}

	for _, ref := range repos {
		if !timeLeft(ctx) {
			note("time budget exhausted before %s", ref.slug())
			break
		}
		var open []ghPR
		checked := false
		after := ""
		for page := 0; ; page++ {
			if !timeLeft(ctx) {
				note("%s: time budget exhausted after %d open PRs", ref.slug(), len(open))
				break
			}
			if page == githubMaxPRPages {
				note("%s: more than %d open PRs; the rest were not checked", ref.slug(), githubMaxPRPages*githubPRPage)
				break
			}
			d, err := c.repo(ref, after)
			if err != nil {
				if ctx.Err() != nil {
					note("%s: time budget exhausted mid-call; open PRs not checked", ref.slug())
				} else {
					note("%s: %v", ref.slug(), err)
				}
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
			out = append(out, p.checkPR(ref, &open[i], in.Now, d, isPersona, isHuman, owns, note)...)
		}
	}
	if len(partial) > 0 {
		return out, &PartialError{Reason: strings.Join(partial, "; ")}
	}
	return out, nil
}

func (p *GitHubPort) checkPR(ref repoRef, pr *ghPR, now time.Time, d githubDurations, isPersona, isHuman map[string]bool, owns func(string, int) (bool, error), note func(string, ...any)) []Finding {
	key := fmt.Sprintf("%s#%d", ref.slug(), pr.Number)
	ours := isPersona[strings.ToLower(pr.Author.Login)]
	head := pr.HeadRefOid
	ci := "none"
	// Fixed-window connections that did not fit mean the review state is
	// not fully known: say so, which makes the port partial.
	if pr.Reviews.PageInfo.truncated() || pr.ReviewThreads.PageInfo.truncated() || pr.ReviewRequests.PageInfo.truncated() || pr.TimelineItems.PageInfo.truncated() {
		note("%s: more than the fetched window of reviews, threads, requests or events; review state not fully checked", key)
	}
	// Push time: the head's newest check suite, else the latest force-push
	// event, else the committer date (pushedDate is always null).
	pushed := pr.CreatedAt
	var forcePushed time.Time
	for _, ev := range pr.TimelineItems.Nodes {
		if ev.Typename == "HeadRefForcePushedEvent" && ev.CreatedAt.After(forcePushed) {
			forcePushed = ev.CreatedAt
		}
	}
	if len(pr.Commits.Nodes) > 0 {
		cm := pr.Commits.Nodes[0].Commit
		switch {
		case len(cm.CheckSuites.Nodes) > 0 && !cm.CheckSuites.Nodes[0].CreatedAt.IsZero():
			pushed = cm.CheckSuites.Nodes[0].CreatedAt
		case !forcePushed.IsZero():
			pushed = forcePushed
		case !cm.CommittedDate.IsZero():
			pushed = cm.CommittedDate
		}
		if forcePushed.After(pushed) {
			pushed = forcePushed
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
	latest := map[string]rv{} // keyed by lower-cased login
	reviewerSawHead, reviewerEver := false, false
	for _, r := range pr.Reviews.Nodes {
		login := strings.ToLower(r.Author.Login)
		if ref.Reviewer != "" && strings.EqualFold(r.Author.Login, ref.Reviewer) {
			reviewerEver = true // a COMMENTED pass is still a pass of the loop
			if r.Commit.Oid == head && r.State == "APPROVED" {
				reviewerSawHead = true
			}
		}
		if r.State == "COMMENTED" || r.State == "PENDING" {
			continue
		}
		if r.State == "DISMISSED" {
			delete(latest, login)
			continue
		}
		latest[login] = rv{r.State, r.Commit.Oid, r.SubmittedAt}
	}
	requested := map[string]time.Time{}
	for _, rr := range pr.ReviewRequests.Nodes {
		if l := strings.ToLower(rr.RequestedReviewer.Login); isHuman[l] {
			requested[l] = pr.CreatedAt
		}
	}
	for _, ev := range pr.TimelineItems.Nodes {
		if l := strings.ToLower(ev.RequestedReviewer.Login); l != "" && requested[l].Before(ev.CreatedAt) {
			if _, ok := requested[l]; ok {
				requested[l] = ev.CreatedAt
			}
		}
	}
	// Deterministic choices over the review map: sorted logins, earliest
	// time wins, the configured approver wins among head approvals.
	logins := make([]string, 0, len(latest))
	for login := range latest {
		if isHuman[login] {
			logins = append(logins, login)
		}
	}
	sort.Strings(logins)
	var humanApprovedHead, humanApprovedOld, humanChanges string
	var approvedAt, changesAt time.Time
	var changesLogins []string
	for _, login := range logins {
		r := latest[login]
		switch {
		case r.state == "APPROVED" && r.oid == head:
			isApprover := ref.Approver != "" && strings.EqualFold(login, ref.Approver)
			if humanApprovedHead == "" || isApprover || (!strings.EqualFold(humanApprovedHead, ref.Approver) && r.at.Before(approvedAt)) {
				humanApprovedHead, approvedAt = login, r.at
			}
		case r.state == "APPROVED":
			if humanApprovedOld == "" || r.at.Before(latest[humanApprovedOld].at) {
				humanApprovedOld = login
			}
		case r.state == "CHANGES_REQUESTED":
			changesLogins = append(changesLogins, login)
			if humanChanges == "" || r.at.Before(changesAt) {
				humanChanges, changesAt = login, r.at
			}
		}
	}
	unresolved := 0
	var oldestThread time.Time
	for _, th := range pr.ReviewThreads.Nodes {
		if th.IsResolved || len(th.Comments.Nodes) == 0 || !isHuman[strings.ToLower(th.Comments.Nodes[0].Author.Login)] {
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
		l = strings.ToLower(l)
		return l == "" || isPersona[l] || isHuman[l] || strings.EqualFold(l, ref.Reviewer)
	}
	var strangers []string
	for _, rr := range pr.ReviewRequests.Nodes {
		if l := rr.RequestedReviewer.Login; !known(l) {
			strangers = append(strangers, l)
		}
	}
	for _, r := range pr.Reviews.Nodes {
		if l := r.Author.Login; !known(l) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			strangers = append(strangers, strings.ToLower(l))
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
	reqLogins := make([]string, 0, len(requested))
	for login := range requested {
		reqLogins = append(reqLogins, login)
	}
	sort.Strings(reqLogins)
	for _, login := range reqLogins {
		at := requested[login]
		if r, ok := latest[login]; ok && r.oid == head && !r.at.Before(at) {
			continue // they reviewed this head after the request; a re-request after a review starts a new wait
		}
		if age := now.Sub(at); age >= d.remind {
			// Keyed per requested human; the fingerprint carries the request
			// time and the remind_after periods since it, and ignores the
			// head: a new push on an unchanged request never re-pings on its
			// own, while a re-request starts a fresh cycle even if a partial
			// port kept the old entry from being evicted.
			f := base("review-waiting", SeverityActionable)
			f.Key = key + "@" + login
			f.Since = at
			f.Fields["human"] = login
			f.Fields["requested_at"] = at.UTC().Format(time.RFC3339)
			f.Suggest = "re-ping " + login + " on " + f.Key
			f.Fingerprint = fmt.Sprintf("%s|%s|%d", login, at.UTC().Format(time.RFC3339), int(age/d.remind)) // once per remind_after per request
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
			f.Fingerprint = fmt.Sprintf("%s|%d|%s", head, unresolved, strings.Join(changesLogins, ","))
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
			if owned, err = owns(ref.Rig, pr.Number); err != nil {
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
