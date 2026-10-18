package sweep

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// --- release-unclosed --------------------------------------------------------

// contains reports whether base is an ancestor of (or equal to) head.
func (c *ghClient) contains(ref repoRef, base, head string) (bool, error) {
	p := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", ref.Owner, ref.Name, base, head)
	if c.trace != nil {
		c.trace("GET " + p)
	}
	if c.dry {
		return false, nil
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.port.apiBase+p+"?per_page=1", nil)
	if err != nil {
		return false, err
	}
	var res struct {
		Status string `json:"status"`
	}
	if err := c.do(req, &res); err != nil {
		return false, err
	}
	return res.Status == "ahead" || res.Status == "identical", nil
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// checkReleases reports merged release-branch PRs in the window with no tag
// reachable from the merge commit.
func (p *GitHubPort) checkReleases(c *ghClient, ref repoRef, data *ghRepoData, patterns []string, now time.Time, d githubDurations, note func(string, ...any)) []Finding {
	type tag struct {
		oid string
		at  time.Time
	}
	var tags []tag
	for _, t := range data.Tags.Nodes {
		tg := tag{oid: t.Target.Oid, at: t.Target.CommittedDate}
		if t.Target.Target != nil {
			tg.oid, tg.at = t.Target.Target.Oid, t.Target.Target.CommittedDate
		}
		tags = append(tags, tg)
	}
	var out []Finding
	for _, pr := range data.Merged.Nodes {
		if pr.MergeCommit == nil || pr.MergedAt.IsZero() || now.Sub(pr.MergedAt) > d.releaseWindow || !matchAny(patterns, pr.BaseRefName) {
			continue
		}
		key := fmt.Sprintf("%s#%d", ref.slug(), pr.Number)
		tagged, checked := false, 0
		for _, t := range tags {
			if t.at.Before(pr.MergedAt) || checked == githubTagsChecked {
				continue
			}
			checked++
			ok, err := c.contains(ref, pr.MergeCommit.Oid, t.oid)
			if err != nil {
				note("%s: compare against tag failed: %v", key, err)
				tagged = true // unknown: never claim it is untagged
				break
			}
			if ok {
				tagged = true
				break
			}
		}
		if tagged {
			continue
		}
		out = append(out, Finding{Kind: "release-unclosed", Key: key, URL: pr.URL, Severity: SeverityActionable, Since: pr.MergedAt,
			Fields:      map[string]string{"merge_commit": pr.MergeCommit.Oid},
			External:    map[string]string{"title": pr.Title, "base": pr.BaseRefName},
			Suggest:     "cut the release tag for " + key,
			Fingerprint: pr.MergeCommit.Oid})
	}
	return out
}

// --- PR-linked Jira kinds ----------------------------------------------------

// ghLinks resolves the Jira issues a PR names (branch name or title) and
// checks the handoff and UAT conventions that need both systems.
type ghLinks struct {
	client                          *jiraClient
	keyRe                           *regexp.Regexp
	reviewers                       map[string]string // GitHub login → Jira account ID
	inReview, inProgress, done, uat string
	uatAfter                        time.Duration
	note                            func(string, ...any)
	cache                           map[string]*jiraIssue
	failed                          bool
}

func (p *GitHubPort) newLinks(ctx context.Context, in Input, note func(string, ...any)) *ghLinks {
	if p.jira == nil || !p.jira.configured(in) {
		return nil
	}
	cfg := in.Settings.JiraSweep()
	client, _, err := p.jira.newClient(ctx, in)
	if err != nil {
		note("PR-linked Jira kinds not checked: %v", err)
		return nil
	}
	projects := make([]string, 0, len(cfg.Projects))
	for _, pr := range cfg.Projects {
		projects = append(projects, regexp.QuoteMeta(pr))
	}
	l := &ghLinks{client: client, keyRe: regexp.MustCompile(`\b(?:` + strings.Join(projects, "|") + `)-\d+\b`),
		reviewers: cfg.Reviewers, inReview: cfg.InReviewStatus, inProgress: cfg.InProgressStatus, done: cfg.DoneStatus, uat: cfg.UATStatus,
		uatAfter: parseDur(cfg.UATSweepAfter, 24*time.Hour), note: note, cache: map[string]*jiraIssue{}}
	for field, def := range map[*string]string{&l.inReview: "In Review", &l.inProgress: "In Progress", &l.done: "Done", &l.uat: "In UAT"} {
		if *field == "" {
			*field = def
		}
	}
	return l
}

// keys returns the issue keys a PR names, deduplicated and sorted.
func (l *ghLinks) keys(pr *ghPR) []string {
	seen := map[string]bool{}
	for _, k := range append(l.keyRe.FindAllString(strings.ToUpper(pr.HeadRefName), -1), l.keyRe.FindAllString(pr.Title, -1)...) {
		seen[k] = true
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// load fetches the named issues in one search and caches them.
func (l *ghLinks) load(keys []string) {
	var missing []string
	for _, k := range keys {
		if _, ok := l.cache[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 || l.failed {
		return
	}
	issues, _, err := l.client.search(fmt.Sprintf(`key in %s`, jqlList(missing)))
	if err != nil {
		l.note("linked Jira issues unavailable: %v", err)
		l.failed = true
		return
	}
	for _, k := range missing {
		l.cache[k] = nil // not found stays nil
	}
	for i := range issues {
		l.cache[issues[i].Key] = &issues[i]
	}
}

func (l *ghLinks) check(ref repoRef, open []ghPR, merged []ghPR, patterns []string, now time.Time, d githubDurations, isHuman map[string]bool) []Finding {
	if l == nil {
		return nil
	}
	var all []string
	for i := range open {
		all = append(all, l.keys(&open[i])...)
	}
	for i := range merged {
		all = append(all, l.keys(&merged[i])...)
	}
	l.load(all)
	if l.failed {
		return nil
	}
	var out []Finding
	base := func(kind, key string, pr *ghPR, is *jiraIssue) Finding {
		return Finding{Kind: kind, Key: key, URL: l.client.origin + "/browse/" + key, Severity: SeverityActionable, Since: pr.CreatedAt,
			Fields:   map[string]string{"pr": fmt.Sprint(pr.Number), "status_category": is.Fields.Status.Category.Key},
			External: map[string]string{"summary": is.Fields.Summary, "status": is.Fields.Status.Name, "pr_title": pr.Title}}
	}
	for i := range open {
		pr := &open[i]
		if pr.IsDraft {
			continue
		}
		// Outstanding request to a listed human with no review on this head.
		var waitingOn string
		reviewed := map[string]bool{}
		var handedBackBy string
		for _, r := range pr.Reviews.Nodes {
			if r.Commit.Oid == pr.HeadRefOid && isHuman[r.Author.Login] {
				reviewed[r.Author.Login] = true
				if r.State == "CHANGES_REQUESTED" {
					handedBackBy = r.Author.Login
				}
			}
		}
		for _, rr := range pr.ReviewRequests.Nodes {
			if login := rr.RequestedReviewer.Login; isHuman[login] && !reviewed[login] {
				waitingOn = login
				break
			}
		}
		for _, key := range l.keys(pr) {
			is := l.cache[key]
			if is == nil {
				continue
			}
			status, assignee := is.Fields.Status.Name, ""
			if is.Fields.Assignee != nil {
				assignee = is.Fields.Assignee.AccountID
			}
			switch {
			case waitingOn != "":
				acct := l.reviewers[waitingOn]
				if !strings.EqualFold(status, l.inReview) || (acct != "" && assignee != acct) {
					f := base("review-handoff", key, pr, is)
					f.Fields["human"] = waitingOn
					if acct != "" {
						f.Fields["account_id"] = acct
					}
					f.Suggest = "move " + key + " to the review status and assign it to " + waitingOn
					f.Fingerprint = pr.HeadRefOid + "|" + status + "|" + assignee
					out = append(out, f)
				}
			case handedBackBy != "" && strings.EqualFold(status, l.inReview):
				f := base("review-handoff", key, pr, is)
				f.Fields["human"] = handedBackBy
				f.Suggest = "move " + key + " back to in progress and assign it to the persona"
				f.Fingerprint = pr.HeadRefOid + "|" + status + "|" + assignee
				out = append(out, f)
			}
		}
	}
	for i := range merged {
		pr := &merged[i]
		if pr.MergedAt.IsZero() || now.Sub(pr.MergedAt) > d.releaseWindow || now.Sub(pr.MergedAt) < l.uatAfter || !matchAny(patterns, pr.BaseRefName) {
			continue
		}
		for _, key := range l.keys(pr) {
			is := l.cache[key]
			if is == nil || !strings.EqualFold(is.Fields.Status.Name, l.done) {
				continue
			}
			f := base("done-not-in-uat", key, pr, is)
			f.Since = pr.MergedAt
			if pr.MergeCommit != nil {
				f.Fields["merge_commit"] = pr.MergeCommit.Oid
			}
			f.Suggest = "move " + key + " to the uat status"
			f.Fingerprint = is.Fields.Status.Name
			if pr.MergeCommit != nil {
				f.Fingerprint += "|" + pr.MergeCommit.Oid
			}
			out = append(out, f)
		}
	}
	return out
}
