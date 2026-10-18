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

// repoSlugOf derives "owner/repo" from a PR's URL.
func repoSlugOf(pr *ghPR) string {
	parts := strings.Split(strings.TrimPrefix(pr.URL, "https://github.com/"), "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return "unknown"
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
	// Newest first: a newer tag on the line contains everything earlier.
	for i, j := 0, len(tags)-1; i < j; i, j = i+1, j-1 {
		tags[i], tags[j] = tags[j], tags[i]
	}
	var out []Finding
	for _, pr := range data.Merged.Nodes {
		if pr.MergeCommit == nil || pr.MergedAt.IsZero() || now.Sub(pr.MergedAt) > d.releaseWindow || !matchAny(patterns, pr.BaseRefName) {
			continue
		}
		key := fmt.Sprintf("%s#%d", ref.slug(), pr.Number)
		tagged, checked := false, 0
		for _, t := range tags {
			if t.oid == pr.MergeCommit.Oid {
				tagged = true // the tag sits on the merge commit itself
				break
			}
			// Tagged-commit dates can precede mergedAt by a little (push
			// merges, clock skew), so the gate is loose; contains() decides.
			if t.at.Before(pr.MergedAt.Add(-24 * time.Hour)) {
				continue
			}
			if checked == githubTagsChecked {
				// Candidates left uncompared (tags on other lines used the
				// cap): unknown for this PR only — an info finding, never
				// "untagged" and never a port-wide partial.
				out = append(out, Finding{Kind: "release-unchecked", Key: key, URL: pr.URL, Severity: SeverityInfo, Since: pr.MergedAt,
					Fields:      map[string]string{"merge_commit": pr.MergeCommit.Oid},
					External:    map[string]string{"title": pr.Title, "base": pr.BaseRefName},
					Suggest:     "check the release tag for " + key,
					Fingerprint: pr.MergeCommit.Oid})
				tagged = true
				break
			}
			if !timeLeft(c.ctx) {
				note("%s: time budget exhausted before the release tag check", key)
				return out
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
	notFound                        map[string]bool // keys Jira rejected as unknown or hidden
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
		reviewers: lowerKeys(cfg.Reviewers), inReview: cfg.InReviewStatus, inProgress: cfg.InProgressStatus, done: cfg.DoneStatus, uat: cfg.UATStatus,
		uatAfter: parseDur(cfg.UATSweepAfter, 24*time.Hour), note: note, cache: map[string]*jiraIssue{}, notFound: map[string]bool{}}
	for field, def := range map[*string]string{&l.inReview: "In Review", &l.inProgress: "In Progress", &l.done: "Done", &l.uat: "In UAT"} {
		if *field == "" {
			*field = def
		}
	}
	return l
}

// maxKeysPerPR bounds how many issue keys one PR (whose title and branch
// are author-chosen text) may contribute to the Jira lookup.
const maxKeysPerPR = 5

// keys returns the issue keys a PR names, shape-checked, deduplicated,
// sorted and capped.
func (l *ghLinks) keys(pr *ghPR) []string {
	seen := map[string]bool{}
	for _, k := range append(l.keyRe.FindAllString(strings.ToUpper(pr.HeadRefName), -1), l.keyRe.FindAllString(pr.Title, -1)...) {
		if jiraKeyRe.MatchString(k) {
			seen[k] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > maxKeysPerPR {
		out = out[:maxKeysPerPR]
	}
	return out
}

// lowerKeys case-folds a login-keyed map.
func lowerKeys(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
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
	for _, k := range missing {
		l.cache[k] = nil // not found stays nil
	}
	issues, _, err := l.client.search(fmt.Sprintf(`key in %s`, jqlList(missing)))
	if err == nil {
		for i := range issues {
			l.cache[issues[i].Key] = &issues[i]
		}
		return
	}
	// One bad key (a typo or an attacker-chosen key in a PR title) fails
	// the batch with a 400: look the keys up one by one so it only drops
	// its own PR, and give up only if every lookup fails.
	outages := 0
	var lastErr error
	for _, k := range missing {
		if !timeLeft(l.client.ctx) {
			l.note("time budget exhausted while looking up linked Jira issues")
			l.failed = true
			return
		}
		one, err := l.client.searchFirst(fmt.Sprintf(`key = %s`, k))
		switch {
		case err != nil && isNotFound(err):
			l.notFound[k] = true // reported per PR as an info finding, never as partial
		case err != nil:
			outages++
			lastErr = err
		case len(one) > 0:
			l.cache[one[0].Key] = &one[0]
		}
	}
	if outages > 0 {
		l.note("%d of %d linked Jira keys could not be looked up: %v", outages, len(missing), lastErr)
	}
	// Only a Jira outage (every lookup a transport, auth, rate-limit or
	// server error) disables the linked kinds for the rest of the sweep.
	if outages > 0 && outages == len(missing) {
		l.failed = true
	}
}

// isNotFound reports Jira's answer for an unknown or hidden key: a 400 (JQL
// validation) or 404 at the status position of the client's error text
// "GET <path>: HTTP <code>: <body>". 401, 403, 429 and 5xx are outages.
func isNotFound(err error) bool {
	return err != nil && notFoundRe.MatchString(err.Error())
}

var notFoundRe = regexp.MustCompile(`^GET \S+: HTTP (400|404):`)

func (l *ghLinks) check(open []ghPR, merged []ghPR, patterns []string, now time.Time, d githubDurations, isPersona, isHuman map[string]bool) []Finding {
	if l == nil {
		return nil
	}
	// Only the persona's own PRs carry the persona's Jira handoffs; a PR
	// anyone else opened must not be able to name an issue into a reassign
	// suggestion.
	ours := func(pr *ghPR) bool { return isPersona[strings.ToLower(pr.Author.Login)] }
	var all []string
	for i := range open {
		if ours(&open[i]) {
			all = append(all, l.keys(&open[i])...)
		}
	}
	for i := range merged {
		if !ours(&merged[i]) {
			continue
		}
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
	missingKeys := func(pr *ghPR) Finding {
		var keys []string
		for _, k := range l.keys(pr) {
			if l.notFound[k] {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return Finding{}
		}
		key := fmt.Sprintf("%s#%d", repoSlugOf(pr), pr.Number)
		return Finding{Kind: "linked-key-missing", Key: key, URL: pr.URL, Severity: SeverityInfo, Since: pr.CreatedAt,
			External:    map[string]string{"keys": strings.Join(keys, ", "), "pr_title": pr.Title},
			Suggest:     "fix the issue key named by " + key,
			Fingerprint: strings.Join(keys, ",")}
	}
	for _, prs := range [][]ghPR{open, merged} {
		for i := range prs {
			if pr := &prs[i]; ours(pr) {
				if f := missingKeys(pr); f.Kind != "" {
					out = append(out, f)
				}
			}
		}
	}
	for i := range open {
		pr := &open[i]
		if pr.IsDraft || !ours(pr) {
			continue
		}
		// Each listed human's latest review state on this head (reviews
		// arrive oldest first, so later entries win); an outstanding
		// request from a human who has not reviewed this head.
		latest := map[string]string{}
		for _, r := range pr.Reviews.Nodes {
			if login := strings.ToLower(r.Author.Login); r.Commit.Oid == pr.HeadRefOid && isHuman[login] && r.State != "COMMENTED" && r.State != "PENDING" {
				latest[login] = r.State
			}
		}
		var waitingOn, handedBackBy string
		for _, rr := range pr.ReviewRequests.Nodes {
			if login := strings.ToLower(rr.RequestedReviewer.Login); isHuman[login] && latest[login] == "" {
				waitingOn = login
				break
			}
		}
		logins := make([]string, 0, len(latest))
		for login := range latest {
			logins = append(logins, login)
		}
		sort.Strings(logins)
		for _, login := range logins {
			if latest[login] == "CHANGES_REQUESTED" {
				handedBackBy = login
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
		if !ours(pr) || pr.MergedAt.IsZero() || now.Sub(pr.MergedAt) > d.releaseWindow || now.Sub(pr.MergedAt) < l.uatAfter || !matchAny(patterns, pr.BaseRefName) {
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
