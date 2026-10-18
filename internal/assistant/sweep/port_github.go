package sweep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/telegraph"
)

// GitHub port defaults (design: docs/design/assistant-role.md, "GitHub port").
const (
	githubDefaultTokenEnv  = "GITHUB_TOKEN" //nolint:gosec // G101: the name of an environment variable, not a secret
	githubPRPage           = 50
	githubMaxPRPages       = 3  // open PRs per repo; more is reported as partial
	githubComparesPerSweep = 30 // compare calls spent on stale approvals (four or five per PR); beyond that the diff is "unknown"
	githubTagsChecked      = 5  // tags compared per merged release PR
)

// patrolWait bounds one patrol-bead lookup (further capped at a quarter of
// the remaining budget).
var patrolWait = 20 * time.Second

var githubDefaultRelease = []string{"release/*"}

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
// review feedback, missing internal review loops, failing CI nobody owns,
// abandoned drafts and untagged release merges, plus the PR-linked Jira
// kinds when the Jira port is configured (port_github_links.go). It only
// reads.
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
	// jira, when configured, supplies issue status for the PR-linked kinds.
	jira *JiraPort
}

// NewGitHubPort returns the production GitHub port.
func NewGitHubPort(townRoot string, getenv func(string) string, jira *JiraPort) *GitHubPort {
	return &GitHubPort{
		jira:     jira,
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
		"{key} only merged its base branch since {human} approved: no new review needed",
		"check why {key} is not merged yet",
		"respond to review comments on {key}",
		"start the internal review loop on {key}",
		"fix ci on {key}",
		"add them to assistant.sweep.github.humans or ignore",
		"cut the release tag for {key}",
		"move {key} to the review status and assign it to {human}",
		"move {key} back to in progress and assign it to the persona",
		"move {key} to the uat status",
	}
}

func (p *GitHubPort) Schema() map[string]Shape {
	return map[string]Shape{
		"human":           ShapeGitHubLogin, // listed human we wait on
		"approver":        ShapeGitHubLogin, // rig's configured approver
		"reviewer":        ShapeGitHubLogin, // rig's configured internal reviewer
		"head":            ShapeSHA,
		"ci":              ShapeEnum, // success | failure | pending | none
		"diff":            ShapeEnum, // approval-stale: unchanged | changed | unknown since the approved head
		"threads":         ShapeInt,  // unresolved review threads
		"requested_at":    ShapeTime,
		"rig":             ShapeEnum,
		"merge_commit":    ShapeSHA,
		"pr":              ShapeInt,           // PR number of a linked issue
		"status_category": ShapeEnum,          // linked Jira issue
		"account_id":      ShapeJiraAccountID, // reviewer's Jira account
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

// UnmarshalJSON decodes a PR and remembers which connections came back null.
func (p *ghPR) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		*p = ghPR{nodeNull: true}
		return nil
	}
	type alias ghPR
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var probe struct {
		Reviews       json.RawMessage `json:"reviews"`
		ReviewThreads json.RawMessage `json:"reviewThreads"`
	}
	_ = json.Unmarshal(b, &probe)
	*p = ghPR(a)
	p.reviewsNull = len(probe.Reviews) == 0 || string(probe.Reviews) == "null"
	p.threadsNull = len(probe.ReviewThreads) == 0 || string(probe.ReviewThreads) == "null"
	return nil
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

type ghTag struct {
	Name   string `json:"name"`
	Target struct {
		Oid           string    `json:"oid"`
		CommittedDate time.Time `json:"committedDate"`
		Target        *struct { // annotated tag → the commit it points at
			Oid           string    `json:"oid"`
			CommittedDate time.Time `json:"committedDate"`
		} `json:"target"`
	} `json:"target"`
}

// ghPR is one pull request as returned by githubRepoQuery. reviewsNull and
// threadsNull record a null connection (a field-level GraphQL error such as
// a FORBIDDEN from a least-privilege token), which must never read as "no
// reviews": such a PR is paged separately or reported as not fully checked.
type ghPR struct {
	nodeNull                 bool // the whole node came back null (a NON_NULL field was denied): unknown PR, never paged
	reviewsNull, threadsNull bool
	MergedAt                 time.Time `json:"mergedAt"`
	MergeCommit              *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
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
	TimelineItems struct { // review-request events only
		PageInfo ghPage           `json:"pageInfo"`
		Nodes    []ghRequestEvent `json:"nodes"`
	} `json:"timelineItems"`
	ForcePushes struct { // newest force-push only; its own window so it never crowds requests out
		Nodes []ghRequestEvent `json:"nodes"`
	} `json:"forcePushes"`
}

type ghRepoData struct {
	Open struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []ghPR `json:"nodes"`
	} `json:"open"`
	Merged struct {
		Nodes []ghPR `json:"nodes"`
	} `json:"merged"`
	Tags struct {
		Nodes []ghTag `json:"nodes"`
	} `json:"tags"`
}

const githubRepoQuery = `query($owner:String!,$name:String!,$after:String,$withTags:Boolean!){
  repository(owner:$owner,name:$name){
    open: pullRequests(states:OPEN, first:50, after:$after, orderBy:{field:UPDATED_AT,direction:DESC}){
      pageInfo{hasNextPage endCursor}
      nodes{
        number title url isDraft createdAt headRefName headRefOid baseRefName author{login}
        reviewRequests(first:20){pageInfo{hasNextPage} nodes{requestedReviewer{... on User{login}}}}
        reviews(last:50){pageInfo{hasPreviousPage} nodes{author{login} state submittedAt commit{oid}}}
        reviewThreads(first:50){pageInfo{hasNextPage} nodes{isResolved comments(first:1){nodes{author{login} createdAt}}}}
        commits(last:1){nodes{commit{committedDate statusCheckRollup{state} checkSuites(last:1){nodes{createdAt}}}}}
        timelineItems(last:30, itemTypes:[REVIEW_REQUESTED_EVENT]){pageInfo{hasPreviousPage} nodes{__typename ... on ReviewRequestedEvent{createdAt requestedReviewer{... on User{login}}}}}
        forcePushes: timelineItems(last:1, itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT]){nodes{__typename ... on HeadRefForcePushedEvent{createdAt}}}
      }
    }
    merged: pullRequests(states:MERGED, first:30, orderBy:{field:UPDATED_AT,direction:DESC}) @include(if:$withTags){
      nodes{number title url mergedAt baseRefName headRefName author{login} mergeCommit{oid}}
    }
    tags: refs(refPrefix:"refs/tags/", last:30, orderBy:{field:TAG_COMMIT_DATE,direction:ASC}) @include(if:$withTags){
      nodes{name target{oid ... on Commit{committedDate} ... on Tag{target{oid ... on Commit{committedDate}}}}}
    }
  }
}`

// githubPRPageQuery pages one PR's reviews and threads when the fixed
// window in githubRepoQuery overflowed, so an outsider cannot turn the whole
// port partial by flooding one PR.
const githubPRPageQuery = `query($owner:String!,$name:String!,$number:Int!,$rcur:String,$tcur:String){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      reviews(first:100, after:$rcur){pageInfo{hasNextPage endCursor} nodes{author{login} state submittedAt commit{oid}}}
      reviewThreads(first:100, after:$tcur){pageInfo{hasNextPage endCursor} nodes{isResolved comments(first:1){nodes{author{login} createdAt}}}}
    }
  }
}`

// ghPRPage is one page of a PR's reviews and threads.
type ghPRPage struct {
	Reviews struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []ghReview `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []ghThread `json:"nodes"`
	} `json:"reviewThreads"`
}

type ghClient struct {
	ctx   context.Context
	port  *GitHubPort
	token string
	trace func(string)
	dry   bool
}

// do sends an authenticated request and decodes the JSON reply into out, or
// hands the raw body over when out is a *[]byte (a non-JSON Accept set by
// the caller is kept). A body at the 8MiB cap is an error, never silently
// truncated data.
func (c *ghClient) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := c.port.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	const capBytes = 8 << 20
	body, _ := io.ReadAll(io.LimitReader(resp.Body, capBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, truncate(string(body), 300))
	}
	if len(body) >= capBytes {
		return fmt.Errorf("%s %s: response exceeds %d bytes", req.Method, req.URL.Path, capBytes)
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = body
		return nil
	}
	return json.Unmarshal(body, out)
}

// prSnapshot is a PR's own diff at one head: GitHub's three-dot compare
// against the base branch, i.e. from the merge base, as raw unified diff
// sections keyed by file, plus that merge base.
type prSnapshot struct {
	mergeBase string
	sections  map[string][]string // new-side path → diff lines
}

// prDiff fetches a PR's diff at a head: one JSON compare for the merge base
// and one diff-text compare for the content (the diff text, unlike the JSON
// file list, carries file modes and rename sources).
func (c *ghClient) prDiff(ref repoRef, base, head string) (*prSnapshot, error) {
	p := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", ref.Owner, ref.Name, url.PathEscape(base), url.PathEscape(head))
	if c.trace != nil {
		c.trace("GET " + p)
	}
	if c.dry {
		return &prSnapshot{sections: map[string][]string{}}, nil
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.port.apiBase+p+"?per_page=1", nil)
	if err != nil {
		return nil, err
	}
	var meta struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := c.do(req, &meta); err != nil {
		return nil, err
	}
	req, err = http.NewRequestWithContext(c.ctx, http.MethodGet, c.port.apiBase+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.diff")
	var raw []byte
	if err := c.do(req, &raw); err != nil {
		return nil, err
	}
	snap := &prSnapshot{mergeBase: meta.MergeBase.SHA, sections: map[string][]string{}}
	for i, sec := range strings.Split(strings.TrimRight(string(raw), "\n"), "\ndiff --git ") {
		if i > 0 {
			sec = "diff --git " + sec
		}
		if sec == "" {
			continue
		}
		lines := strings.Split(sec, "\n")
		key := sectionPath(lines)
		if _, dup := snap.sections[key]; dup {
			return nil, fmt.Errorf("ambiguous diff path %q", key) // a decoy path containing " b/": compare as unknown
		}
		snap.sections[key] = lines
	}
	return snap, nil
}

// sectionPath returns the path a diff section describes. Git does not quote
// spaces, so the "diff --git a/X b/Y" header is ambiguous for a path that
// contains " b/"; the "+++ b/" line read before the first hunk is not (a
// hunk body line "++ b/…" would render as "+++ b/…", hence the cut-off).
// Git appends a tab to a "+++" path that contains a space; it is trimmed so
// the key matches the API's file names. A deletion ("deleted file mode"
// header, which git writes for text, binary and empty files alike) is keyed
// apart from a creation of the same path, so a file-to-symlink typechange
// (two sections under one header) is not a duplicate. An entry
// without a "+++" line (binary, mode-only, rename-only) uses its "rename to"
// line, then the header's last " b/".
func sectionPath(lines []string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, "@@") {
			break
		}
		if strings.HasPrefix(l, "deleted file mode ") || l == "+++ /dev/null" {
			return headerPath(lines[0]) + " (deleted)"
		}
		if strings.HasPrefix(l, "+++ b/") {
			return strings.TrimSuffix(l[len("+++ b/"):], "\t")
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "@@") {
			break
		}
		if strings.HasPrefix(l, "rename to ") {
			return l[len("rename to "):]
		}
	}
	return headerPath(lines[0])
}

func headerPath(header string) string {
	if i := strings.LastIndex(header, " b/"); i >= 0 {
		return header[i+3:]
	}
	return header
}

// touchedBetween lists the files the base branch changed between two merge
// bases (a JSON compare; names on both sides of a rename).
func (c *ghClient) touchedBetween(ref repoRef, from, to string) (map[string]bool, error) {
	p := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", ref.Owner, ref.Name, url.PathEscape(from), url.PathEscape(to))
	if c.trace != nil {
		c.trace("GET " + p)
	}
	if c.dry {
		return map[string]bool{}, nil
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.port.apiBase+p+"?per_page=1", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := c.do(req, &res); err != nil {
		return nil, err
	}
	if len(res.Files) >= 300 {
		return nil, fmt.Errorf("base moved 300 or more files; not comparable")
	}
	out := map[string]bool{}
	for _, f := range res.Files {
		out[f.Filename] = true
		if f.PreviousFilename != "" {
			out[f.PreviousFilename] = true
		}
	}
	return out, nil
}

// normalizeDiff renders a snapshot for comparison. It keeps every line that
// says what the change is — file headers, modes, renames, hunk bodies with
// their function context and line numbers, so a moved hunk is a change —
// and drops what moves with the merge base: blob ids always, and in files
// the base itself touched between the two merge bases the hunk line numbers
// are reduced to their old-to-new shift, which a base-only move does not
// alter. A file entry with no hunk body (a binary) keeps its head-side blob
// id, the only identity it has, so a binary swap is a change.
func normalizeDiff(snap *prSnapshot, baseTouched map[string]bool) []string {
	paths := make([]string, 0, len(snap.sections))
	for p := range snap.sections {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		lines := snap.sections[p]
		hasHunk := false
		for _, l := range lines {
			if strings.HasPrefix(l, "@@") {
				hasHunk = true
				break
			}
		}
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line, "index ") && hasHunk:
				continue // text: the hunks say what changed; blob ids move with the merge base
			case strings.HasPrefix(line, "index "):
				line = "blob " + headBlob(line)
			case strings.HasPrefix(line, "@@") && baseTouched[p]:
				line = shiftOnly(line)
			}
			out = append(out, line)
		}
	}
	return out
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$`)

// shiftOnly rewrites a hunk header to its old-to-new line shift and context.
func shiftOnly(line string) string {
	m := hunkRe.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	oldStart, _ := strconv.Atoi(m[1])
	newStart, _ := strconv.Atoi(m[2])
	return fmt.Sprintf("@@ shift=%d @@%s", newStart-oldStart, m[3])
}

// headBlob returns the head-side blob id of an "index <base>..<head> [mode]" line.
func headBlob(index string) string {
	rest := strings.TrimPrefix(index, "index ")
	if f := strings.Fields(rest); len(f) > 0 {
		rest = f[0]
	}
	if i := strings.Index(rest, ".."); i >= 0 {
		return rest[i+2:]
	}
	return rest
}

// repo fetches one page of a repository's open PRs. A field-level GraphQL
// error with the repository still present (a least-privilege token denied
// one field) keeps the data and is returned as a warning; the PRs whose
// review connections came back null are paged separately by the caller.
func (c *ghClient) repo(ref repoRef, after string, withTags bool) (*ghRepoData, string, error) {
	if c.trace != nil {
		c.trace(fmt.Sprintf("POST /graphql repository(%s) open PRs after=%q merged+tags=%v", ref.slug(), after, withTags))
	}
	if c.dry {
		return &ghRepoData{}, "", nil
	}
	vars := map[string]any{"owner": ref.Owner, "name": ref.Name, "withTags": withTags}
	if after != "" {
		vars["after"] = after
	}
	body, _ := json.Marshal(map[string]any{"query": githubRepoQuery, "variables": vars})
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.port.apiBase+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	var res struct {
		Data struct {
			Repository *struct {
				Open       json.RawMessage `json:"open"`
				ghRepoData                 // value-embedded: json cannot allocate an embedded pointer to an unexported type
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(req, &res); err != nil {
		return nil, "", err
	}
	if res.Data.Repository == nil || len(res.Data.Repository.Open) == 0 || string(res.Data.Repository.Open) == "null" {
		msg := "repository not found or not accessible"
		if len(res.Errors) > 0 {
			msg = "graphql: " + res.Errors[0].Message
		}
		return nil, "", fmt.Errorf("%s: %s", ref.slug(), msg)
	}
	data := &res.Data.Repository.ghRepoData
	if err := json.Unmarshal(res.Data.Repository.Open, &data.Open); err != nil {
		return nil, "", err
	}
	warn := ""
	if len(res.Errors) > 0 {
		warn = fmt.Sprintf("%s: graphql: %s (data kept; affected PRs paged or marked not fully checked)", ref.slug(), res.Errors[0].Message)
	}
	return data, warn, nil
}

// prPage fetches one page of a PR's reviews and threads.
func (c *ghClient) prPage(ref repoRef, number int, rcur, tcur string) (*ghPRPage, error) {
	if c.trace != nil {
		c.trace(fmt.Sprintf("POST /graphql repository(%s) pullRequest(%d) reviews/threads page", ref.slug(), number))
	}
	vars := map[string]any{"owner": ref.Owner, "name": ref.Name, "number": number}
	if rcur != "" {
		vars["rcur"] = rcur
	}
	if tcur != "" {
		vars["tcur"] = tcur
	}
	body, _ := json.Marshal(map[string]any{"query": githubPRPageQuery, "variables": vars})
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.port.apiBase+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					Reviews       *json.RawMessage `json:"reviews"`
					ReviewThreads *json.RawMessage `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(req, &res); err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("%s#%d: graphql: %s", ref.slug(), number, res.Errors[0].Message)
	}
	pr := res.Data.Repository.PullRequest
	// A null connection is a field-level failure, never an empty page.
	if pr == nil || pr.Reviews == nil || pr.ReviewThreads == nil {
		return nil, fmt.Errorf("%s#%d: reviews or threads missing from the page", ref.slug(), number)
	}
	var page ghPRPage
	if err := json.Unmarshal(*pr.Reviews, &page.Reviews); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(*pr.ReviewThreads, &page.ReviewThreads); err != nil {
		return nil, err
	}
	return &page, nil
}

// fillPR replaces a PR's reviews and threads with the complete, paged lists
// when the fixed window overflowed. It returns false when paging was cut
// (budget or page cap), in which case the caller notes the PR as not fully
// checked.
func (c *ghClient) fillPR(ctx context.Context, ref repoRef, pr *ghPR) (bool, error) {
	const maxPages = 10
	var reviews []ghReview
	var threads []ghThread
	rcur, tcur := "", ""
	moreR, moreT := true, true
	for page := 0; moreR || moreT; page++ {
		if page == maxPages || !timeLeft(ctx) {
			return false, nil
		}
		pg, err := c.prPage(ref, pr.Number, rcur, tcur)
		if err != nil {
			return false, err
		}
		if moreR {
			reviews = append(reviews, pg.Reviews.Nodes...)
			moreR, rcur = pg.Reviews.PageInfo.HasNextPage, pg.Reviews.PageInfo.EndCursor
		}
		if moreT {
			threads = append(threads, pg.ReviewThreads.Nodes...)
			moreT, tcur = pg.ReviewThreads.PageInfo.HasNextPage, pg.ReviewThreads.PageInfo.EndCursor
		}
	}
	pr.Reviews.Nodes, pr.Reviews.PageInfo = reviews, ghPage{}
	pr.ReviewThreads.Nodes, pr.ReviewThreads.PageInfo = threads, ghPage{}
	return true, nil
}

// --- the sweep -------------------------------------------------------------

type githubDurations struct {
	remind, approvedUnmerged, respond, reviewLoop, ciFailing, releaseWindow, stale time.Duration
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
		releaseWindow:    parseDur(cfg.ReleaseWindow, 14*24*time.Hour),
		stale:            parseDur(cfg.StaleAfter, 7*24*time.Hour),
	}
	isHuman := lowerSet(cfg.Humans)
	for _, ref := range repos {
		if ref.Approver != "" {
			isHuman[strings.ToLower(ref.Approver)] = true // a rig's configured approver is human by definition
		}
	}
	for l := range isPersona {
		delete(isHuman, l) // the persona is never a human reviewer, even if listed
	}
	var out []Finding
	var partial []string
	note := func(format string, args ...any) { partial = append(partial, fmt.Sprintf(format, args...)) }
	// diffSince tells whether a PR's own diff changed between the head a
	// human approved and the current head: "unchanged" means only the base
	// branch was merged in. Two compare calls per stale approval, capped
	// per sweep; anything unanswerable is "unknown" (and asks for review).
	compares := 0
	diffSince := func(ref repoRef, base, old, head string) (string, error) {
		if compares >= githubComparesPerSweep {
			return "unknown", fmt.Errorf("compare budget of %d calls spent", githubComparesPerSweep) // not an outage: "unknown" already asks for review
		}
		compares += 4
		was, err := c.prDiff(ref, base, old)
		if err != nil {
			return "unknown", err
		}
		is, err := c.prDiff(ref, base, head)
		if err != nil {
			return "unknown", err
		}
		touched := map[string]bool{}
		if was.mergeBase != is.mergeBase {
			compares++
			if touched, err = c.touchedBetween(ref, was.mergeBase, is.mergeBase); err != nil {
				return "unknown", err
			}
		}
		if slices.Equal(normalizeDiff(was, touched), normalizeDiff(is, touched)) {
			return "unchanged", nil
		}
		return "changed", nil
	}
	// One patrol-bead lookup per rig per sweep, run under the budget: a
	// hung bd turns into a note, never into the runner timeout. Failures
	// are cached too, so a rig is tried once.
	type patrolResult struct {
		set map[int]bool
		err error
	}
	patrol := map[string]patrolResult{}
	var bdDown error // set by the first timed-out lookup: every rig shares one Dolt server
	owns := func(rig string, n int) (bool, error) {
		res, ok := patrol[rig]
		if !ok {
			if bdDown != nil {
				res = patrolResult{err: bdDown}
			} else if !timeLeft(ctx) {
				res = patrolResult{err: fmt.Errorf("time budget exhausted")}
			} else {
				// Bound each lookup on its own timer so a wedged bd costs one
				// rig its dedup, not every later repo its findings.
				wait := patrolWait
				if dl, ok := ctx.Deadline(); ok {
					if quarter := time.Until(dl) / 4; quarter < wait {
						wait = quarter
					}
				}
				done := make(chan patrolResult, 1)
				go func() {
					set, err := p.patrolBeads(rig)
					done <- patrolResult{set, err}
				}()
				select {
				case res = <-done:
				case <-ctx.Done():
					res = patrolResult{err: fmt.Errorf("time budget exhausted waiting for bd")}
				case <-time.After(wait):
					bdDown = fmt.Errorf("bd unavailable this sweep: did not answer within %s", wait.Round(time.Millisecond))
					res = patrolResult{err: bdDown}
				}
			}
			if res.set == nil {
				res.set = map[int]bool{}
			}
			patrol[rig] = res
		}
		return res.set[n], res.err
	}

	links := p.newLinks(ctx, in, note)
	for _, ref := range repos {
		if !timeLeft(ctx) {
			note("time budget exhausted before %s", ref.slug())
			break
		}
		var open []ghPR
		var first *ghRepoData
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
			d, warn, err := c.repo(ref, after, page == 0)
			if err != nil {
				if ctx.Err() != nil {
					note("%s: time budget exhausted mid-call; open PRs not checked", ref.slug())
				} else {
					note("%s: %v", ref.slug(), err)
				}
				break
			}
			if warn != "" {
				note("%s", warn)
			}
			if page == 0 {
				first = d
			}
			open = append(open, d.Open.Nodes...)
			if !d.Open.PageInfo.HasNextPage {
				break
			}
			after = d.Open.PageInfo.EndCursor
		}
		if first == nil {
			continue
		}
		for i := range open {
			pr := &open[i]
			if pr.nodeNull {
				// A denied NON_NULL field nulls the whole node: GitHub gave us no
				// number to page, so name the slot and move on.
				note("%s: open PR at index %d came back null (a field was denied); not checked", ref.slug(), i)
				continue
			}
			if pr.Reviews.PageInfo.truncated() || pr.ReviewThreads.PageInfo.truncated() || pr.reviewsNull || pr.threadsNull {
				// Page this one PR instead of declaring the whole port partial
				// over a window nobody has read.
				if complete, err := c.fillPR(ctx, ref, pr); err != nil || !complete {
					note("%s#%d: reviews or threads could not be paged in full; review state not fully checked", ref.slug(), pr.Number)
				}
			}
			out = append(out, p.checkPR(ref, pr, in.Now, d, isPersona, isHuman, owns, diffSince, note)...)
		}
		patterns := cfg.ReleaseBranches[ref.slug()]
		if patterns == nil {
			patterns = cfg.ReleaseBranches["*"]
		}
		if patterns == nil {
			patterns = githubDefaultRelease
		}
		out = append(out, p.checkReleases(c, ref, first, patterns, in.Now, d, note)...)
		out = append(out, links.check(ref, open, first.Merged.Nodes, patterns, in.Now, d, isHuman)...)
	}
	if len(partial) > 0 {
		return out, &PartialError{Reason: strings.Join(partial, "; ")}
	}
	return out, nil
}

func (p *GitHubPort) checkPR(ref repoRef, pr *ghPR, now time.Time, d githubDurations, isPersona, isHuman map[string]bool, owns func(string, int) (bool, error), diffSince func(repoRef, string, string, string) (string, error), note func(string, ...any)) []Finding {
	key := fmt.Sprintf("%s#%d", ref.slug(), pr.Number)
	ours := isPersona[strings.ToLower(pr.Author.Login)]
	head := pr.HeadRefOid
	ci := "none"
	// Reviews and threads are paged by the caller when their window
	// overflowed; only the request windows (author-controlled) remain fixed.
	if pr.ReviewRequests.PageInfo.truncated() || pr.TimelineItems.PageInfo.truncated() {
		note("%s: more than the fetched window of review requests; request times not fully checked", key)
	}
	// Push time: the head's newest check suite, else the later of the latest
	// force-push event and the committer date (pushedDate is always null).
	pushed := pr.CreatedAt
	var forcePushed time.Time
	for _, ev := range pr.ForcePushes.Nodes {
		if ev.CreatedAt.After(forcePushed) {
			forcePushed = ev.CreatedAt
		}
	}
	if len(pr.Commits.Nodes) > 0 {
		cm := pr.Commits.Nodes[0].Commit
		switch {
		case len(cm.CheckSuites.Nodes) > 0 && !cm.CheckSuites.Nodes[0].CreatedAt.IsZero():
			pushed = cm.CheckSuites.Nodes[0].CreatedAt
		case forcePushed.After(cm.CommittedDate):
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
	strangerSet := map[string]bool{}
	for _, rr := range pr.ReviewRequests.Nodes {
		if l := strings.ToLower(rr.RequestedReviewer.Login); !known(l) {
			strangerSet[l] = true
		}
	}
	for _, r := range pr.Reviews.Nodes {
		if l := strings.ToLower(r.Author.Login); !known(l) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			strangerSet[l] = true
		}
	}
	var strangers []string
	for l := range strangerSet {
		strangers = append(strangers, l)
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
		// A head that only merged the base branch since the approval
		// leaves the PR's own diff unchanged: nothing to re-review. Any
		// material change, or a compare that cannot answer, asks for a
		// fresh approval.
		// A compare that cannot answer (error, too large, budget) is
		// "unknown": the advice is already to re-approve, and a persistent
		// failure must not make the port partial — and stop eviction — on
		// every sweep; the reason is fenced so it is not lost.
		approvedOid := latest[humanApprovedOld].oid
		diff, err := diffSince(ref, pr.BaseRefName, approvedOid, head)
		if err != nil {
			f.External["diff_error"] = err.Error()
		}
		f.Fields["diff"] = diff
		if diff == "unchanged" {
			f.Suggest = key + " only merged its base branch since " + humanApprovedOld + " approved: no new review needed"
		} else {
			f.Suggest = "ask " + humanApprovedOld + " to re-approve " + key
		}
		// Fingerprint the pair of heads, not the verdict: for a fixed
		// approved head and current head the verdict only varies with
		// errors, and a transient compare error must not re-page an
		// acknowledged item.
		f.Fingerprint = head + "|" + approvedOid
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
