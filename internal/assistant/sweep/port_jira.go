package sweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// Jira port defaults (design: docs/design/assistant-role.md, "Jira port").
const (
	jiraDefaultTokenEnv  = "JIRA_API_TOKEN" //nolint:gosec // G101: the name of an environment variable, not a secret
	jiraPageSize         = 100
	jiraMaxPages         = 10  // per search; more is reported as partial
	jiraMaxCommentIssues = 150 // issues whose comments are fetched per run
	jiraCommentPage      = 50
)

var (
	jiraDefaultActive = []string{"To Do", "Research", "In Progress", "In Review"}
	jiraDefaultWIP    = []string{"In Progress", "Research"}
)

type jiraDurations struct {
	unanswered, chase, statusUpdate, offBoard, stale time.Duration
}

// JiraPort reports unanswered threads, unchased questions, assigned work
// without a status note, off-board issues, WIP over the limit and stale
// release waves for the persona's Jira projects. It only reads.
type JiraPort struct {
	// persona returns the persona's Jira account IDs (telegraph.toml).
	persona func() ([]string, error)
	getenv  func(string) string
	client  *http.Client
	// base turns the configured site into the API origin; tests point it
	// at an httptest server.
	base func(site string) string
}

// NewJiraPort returns the production Jira port.
func NewJiraPort(townRoot string, getenv func(string) string) *JiraPort {
	return &JiraPort{
		persona: func() ([]string, error) {
			cfg, err := telegraph.LoadWithDefaults(telegraph.DefaultPath(townRoot))
			if err != nil {
				return nil, err
			}
			var ids []string
			for _, id := range cfg.Telegraph.Mayor.JiraAccountIDs {
				if id = strings.TrimSpace(id); id != "" {
					ids = append(ids, id)
				}
			}
			return ids, nil
		},
		getenv: getenv,
		client: &http.Client{Timeout: 30 * time.Second},
		base:   func(site string) string { return "https://" + site },
	}
}

func (p *JiraPort) Name() string { return "jira" }

func (p *JiraPort) Suggestions() []string {
	return []string{
		"reply on {key}",
		"add {account_id} to assistant.sweep.jira.humans or .bots",
		"chase {key}",
		"note on {key} why it waits",
		"status note on {key}",
		"move {key} onto board {board} (jira-move-to-board)",
		"finish or hand off before starting more",
		"review release wave {key}",
	}
}

func (p *JiraPort) Schema() map[string]Shape {
	return map[string]Shape{
		"status_category": ShapeEnum,          // new | indeterminate | done
		"comment_id":      ShapeInt,           // Jira comment ID
		"human":           ShapeJiraAccountID, // who we wait on
		"account_id":      ShapeJiraAccountID, // unknown actor
		"count":           ShapeInt,
		"limit":           ShapeInt,
		"board":           ShapeInt,
	}
}

// --- wire types -----------------------------------------------------------

type jiraTime time.Time

func (t *jiraTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = jiraTime(v)
			return nil
		}
	}
	return fmt.Errorf("unparseable Jira time %q", s)
}

type jiraUser struct {
	AccountID string `json:"accountId"`
}

type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string   `json:"summary"`
		Created jiraTime `json:"created"`
		Updated jiraTime `json:"updated"`
		Status  struct {
			Name     string `json:"name"`
			Category struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		Assignee *jiraUser `json:"assignee"`
		Reporter *jiraUser `json:"reporter"`
		Creator  *jiraUser `json:"creator"`
	} `json:"fields"`
}

type jiraComment struct {
	ID      string          `json:"id"`
	Author  jiraUser        `json:"author"`
	Created jiraTime        `json:"created"`
	Body    json.RawMessage `json:"body"` // Atlassian Document Format
}

// adfWalk flattens an ADF body to text and collects mentioned account IDs.
func adfWalk(raw json.RawMessage) (text string, mentions []string) {
	var node struct {
		Type    string            `json:"type"`
		Text    string            `json:"text"`
		Attrs   map[string]string `json:"attrs"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return "", nil
	}
	var b strings.Builder
	switch node.Type {
	case "text":
		b.WriteString(node.Text)
	case "mention":
		if id := node.Attrs["id"]; id != "" {
			mentions = append(mentions, id)
		}
	}
	for _, c := range node.Content {
		t, m := adfWalk(c)
		b.WriteString(t)
		mentions = append(mentions, m...)
	}
	return b.String(), mentions
}

// --- HTTP ---------------------------------------------------------------

type jiraClient struct {
	ctx    context.Context
	port   *JiraPort
	origin string
	email  string
	token  string
	trace  func(string)
	dry    bool
}

func (c *jiraClient) get(path string, q url.Values, out any) error {
	u := c.origin + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if c.trace != nil {
		c.trace("GET " + path + " " + strings.ReplaceAll(q.Encode(), "+", " "))
	}
	if c.dry {
		return nil
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.port.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 300))
	}
	return json.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// search runs a JQL query through every page (bounded by jiraMaxPages).
func (c *jiraClient) search(jql string) ([]jiraIssue, bool, error) {
	return c.searchN(jql, jiraPageSize, jiraMaxPages)
}

// searchFirst returns at most the first issue of a query in one call.
func (c *jiraClient) searchFirst(jql string) ([]jiraIssue, error) {
	issues, _, err := c.searchN(jql, 1, 1)
	return issues, err
}

func (c *jiraClient) searchN(jql string, pageSize, maxPages int) ([]jiraIssue, bool, error) {
	var all []jiraIssue
	token := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"jql": {jql}, "maxResults": {fmt.Sprint(pageSize)},
			"fields": {"summary,status,assignee,reporter,creator,created,updated"}}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		var res struct {
			Issues        []jiraIssue `json:"issues"`
			NextPageToken string      `json:"nextPageToken"`
			IsLast        bool        `json:"isLast"`
		}
		if err := c.get("/rest/api/3/search/jql", q, &res); err != nil {
			return all, false, err
		}
		all = append(all, res.Issues...)
		if c.dry || res.IsLast || res.NextPageToken == "" {
			return all, true, nil
		}
		token = res.NextPageToken
	}
	return all, maxPages == 1, nil
}

// jiraKeyRe is the shape an issue key must have before it is printed outside
// a fence or interpolated into JQL.
var jiraKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}-\d{1,9}$`)

// timeLeft reports whether the port still has budget for another call;
// when the deadline is near it stops and reports what it has as partial.
func timeLeft(ctx context.Context) bool { return ctx.Err() == nil }

// budgetMargin is how long before the port deadline the Jira calls stop,
// so an in-flight request is cut with time left to return a partial result.
var budgetMargin = 15 * time.Second

// withBudget derives the context Jira calls run under: the port deadline
// minus budgetMargin. In-flight calls are canceled at the margin instead of
// running into the runner's timeout, which would discard every finding.
func withBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, dl.Add(-budgetMargin))
	}
	return context.WithCancel(ctx)
}

func (c *jiraClient) comments(key string) ([]jiraComment, error) {
	var res struct {
		Comments []jiraComment `json:"comments"`
	}
	err := c.get("/rest/api/3/issue/"+url.PathEscape(key)+"/comment",
		url.Values{"orderBy": {"-created"}, "maxResults": {fmt.Sprint(jiraCommentPage)}}, &res)
	sort.SliceStable(res.Comments, func(i, j int) bool {
		return time.Time(res.Comments[i].Created).After(time.Time(res.Comments[j].Created))
	})
	return res.Comments, err
}

// backlog returns the keys sitting in the board's backlog (off the board).
func (c *jiraClient) backlog(boardID int) (map[string]bool, bool, error) {
	keys := map[string]bool{}
	for start, page := 0, 0; page < 20; page++ {
		var res struct {
			Issues []struct {
				Key string `json:"key"`
			} `json:"issues"`
			Total int `json:"total"`
		}
		if err := c.get(fmt.Sprintf("/rest/agile/1.0/board/%d/backlog", boardID),
			url.Values{"startAt": {fmt.Sprint(start)}, "maxResults": {fmt.Sprint(jiraPageSize)}, "fields": {"key"}}, &res); err != nil {
			return keys, false, err
		}
		for _, i := range res.Issues {
			keys[i.Key] = true
		}
		start += len(res.Issues)
		if c.dry || len(res.Issues) == 0 || start >= res.Total {
			return keys, true, nil
		}
	}
	return keys, false, nil
}

// --- the sweep -------------------------------------------------------------

func jqlList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = `"` + strings.ReplaceAll(s, `"`, ``) + `"`
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

func jqlMinutes(d time.Duration) string { return fmt.Sprintf(`"-%dm"`, int(d.Minutes())) }

func parseDur(raw string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

func (p *JiraPort) Sweep(ctx context.Context, in Input) ([]Finding, error) {
	cfg := in.Settings.JiraSweep()
	ctx, cancel := withBudget(ctx)
	defer cancel()
	if cfg.Site == "" || len(cfg.Projects) == 0 {
		return nil, fmt.Errorf("jira port not configured: set assistant.sweep.jira.site and .projects")
	}
	ids, err := p.persona()
	if err != nil {
		return nil, fmt.Errorf("reading telegraph.toml: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no persona account ID configured (telegraph.mayor.jira_account_ids); jira_usernames is not used by the sweep")
	}
	tokenEnv := cfg.TokenEnv
	if tokenEnv == "" {
		tokenEnv = jiraDefaultTokenEnv
	}
	c := &jiraClient{ctx: ctx, port: p, origin: p.base(cfg.Site), email: cfg.Email, token: p.getenv(tokenEnv), trace: in.Trace, dry: in.DryRun}
	if !in.DryRun && (c.token == "" || c.email == "") {
		return nil, fmt.Errorf("jira credentials missing: %s and assistant.sweep.jira.email are required", tokenEnv)
	}
	d := jiraDurations{
		unanswered:   parseDur(cfg.UnansweredAfter, 4*time.Hour),
		chase:        parseDur(cfg.ChaseAfter, 24*time.Hour),
		statusUpdate: parseDur(cfg.StatusUpdateAfter, 72*time.Hour),
		offBoard:     parseDur(cfg.OffBoardWindow, 7*24*time.Hour),
		stale:        parseDur(cfg.StaleAfter, 72*time.Hour),
	}
	active := cfg.ActiveStatuses
	if len(active) == 0 {
		active = jiraDefaultActive
	}
	wipStatuses := cfg.WIPStatuses
	if len(wipStatuses) == 0 {
		wipStatuses = jiraDefaultWIP
	}
	wipLimit := cfg.WIPLimit
	if wipLimit <= 0 {
		wipLimit = 4
	}
	isPersona, isHuman, isBot := set(ids), set(cfg.Humans), set(cfg.Bots)
	projects := jqlList(cfg.Projects)
	people := jqlList(ids)
	browse := p.base(cfg.Site) + "/browse/"

	var partial []string
	note := func(format string, args ...any) { partial = append(partial, fmt.Sprintf(format, args...)) }

	// 1. Issues the persona is involved in (open), plus anything recently
	//    updated in the projects so @-mentions by others are seen.
	involvedJQL := fmt.Sprintf(`project in %s AND ((statusCategory != Done AND (assignee in %s OR reporter in %s OR creator in %s OR watcher in %s)) OR updated >= %s) ORDER BY updated DESC`,
		projects, people, people, people, people, jqlMinutes(2*d.unanswered))
	issues, complete, err := c.search(involvedJQL)
	if err != nil {
		return nil, err
	}
	if !complete {
		note("involved-issue search truncated after %d pages", jiraMaxPages)
	}
	// 1b. Issues any persona account watches: watches.isWatching only
	//     answers for the token's user, so membership comes from JQL.
	watched := map[string]bool{}
	wIssues, wOK, err := c.search(fmt.Sprintf(`project in %s AND statusCategory != Done AND watcher in %s`, projects, people))
	if err != nil {
		note("watcher search failed (watched issues get mention checks only): %v", err)
	} else if !wOK {
		note("watcher search truncated after %d pages", jiraMaxPages)
	}
	for _, i := range wIssues {
		watched[i.Key] = true
	}
	// 2. Issues whose status changed recently (a transition counts as a
	//    status update even without a comment).
	transitioned := map[string]bool{}
	tIssues, tComplete, err := c.search(fmt.Sprintf(`project in %s AND status changed after %s`, projects, jqlMinutes(d.statusUpdate)))
	if err != nil {
		note("status-change search failed: %v", err)
	} else if !tComplete {
		note("status-change search truncated after %d pages", jiraMaxPages)
	}
	for _, i := range tIssues {
		transitioned[i.Key] = true
	}
	// 3. Backlog membership (off-board).
	var backlog map[string]bool
	if cfg.BoardID > 0 {
		var ok bool
		backlog, ok, err = c.backlog(cfg.BoardID)
		if err != nil {
			note("board %d backlog unavailable (off-board not checked): %v", cfg.BoardID, err)
			backlog = nil
		} else if !ok {
			note("board %d backlog truncated", cfg.BoardID)
		}
	}
	// 4. Release-wave parents.
	waves, wComplete, err := c.search(fmt.Sprintf(`project in %s AND statusCategory != Done AND summary ~ "\"Release wave\"" ORDER BY created DESC`, projects))
	if err != nil {
		note("release-wave search failed: %v", err)
	} else if !wComplete {
		note("release-wave search truncated after %d pages", jiraMaxPages)
	}
	if in.DryRun {
		c.trace("GET /rest/api/3/issue/{key}/comment orderBy=-created maxResults=" + fmt.Sprint(jiraCommentPage) + " (per involved issue)")
		c.trace(`GET /rest/api/3/search/jql jql=parent = {wave} ORDER BY updated DESC maxResults=1 (per release wave)`)
		return nil, nil
	}

	var out []Finding
	wip := 0
	for n, is := range issues {
		if n >= jiraMaxCommentIssues {
			note("comments fetched for the first %d of %d issues", jiraMaxCommentIssues, len(issues))
			break
		}
		if !timeLeft(ctx) {
			note("time budget exhausted after %d of %d issues", n, len(issues))
			break
		}
		f := is.Fields
		assignedToPersona := f.Assignee != nil && isPersona[f.Assignee.AccountID]
		// Only issues the persona is involved in get the full set of checks;
		// the rest came in through the recently-updated clause and are
		// scanned for @-mentions of the persona only.
		involved := assignedToPersona || watched[is.Key] ||
			(f.Reporter != nil && isPersona[f.Reporter.AccountID]) || (f.Creator != nil && isPersona[f.Creator.AccountID])
		status := f.Status.Name
		if assignedToPersona && contains(wipStatuses, status) {
			wip++
		}
		if !jiraKeyRe.MatchString(is.Key) {
			// Never send a malformed key down the REST path; report it as a
			// config/data oddity instead (the raw key is fenced by jiraBase).
			fd := jiraBase(is, browse)("malformed-key", SeverityInfo)
			fd.Since = time.Time(f.Updated)
			fd.Fingerprint = "malformed"
			out = append(out, fd)
			continue
		}
		comments, err := c.comments(is.Key)
		if err != nil {
			note("%s: comments unavailable: %v", is.Key, err)
			continue
		}
		base := jiraBase(is, browse)
		// Comments after the persona's last word, newest first.
		var lastPersona time.Time
		var lastPersonaID, lastPersonaText string
		for _, cm := range comments {
			if isPersona[cm.Author.AccountID] {
				lastPersona, lastPersonaID = time.Time(cm.Created), cm.ID
				lastPersonaText, _ = adfWalk(cm.Body)
				break
			}
		}
		var waiting, unknown *jiraComment
		var waitingHuman string
		humanAfter := false // a listed human spoke after the persona
		for i := range comments {
			cm := &comments[i]
			at := time.Time(cm.Created)
			if !at.After(lastPersona) {
				break
			}
			_, mentions := adfWalk(cm.Body)
			mentioned := mentionsAny(mentions, isPersona)
			switch {
			case isPersona[cm.Author.AccountID]:
			case isBot[cm.Author.AccountID]:
			case isHuman[cm.Author.AccountID]:
				humanAfter = true
				if waiting == nil && (involved || mentioned) {
					waiting, waitingHuman = cm, cm.Author.AccountID
				}
			default:
				if mentioned && waiting == nil {
					waiting = cm
				}
				if unknown == nil && involved {
					unknown = cm
				}
			}
		}
		if waiting != nil && in.Now.Sub(time.Time(waiting.Created)) >= d.unanswered {
			fd := base("unanswered-comment", SeverityActionable)
			fd.Since = time.Time(waiting.Created)
			fd.Fields["comment_id"] = waiting.ID
			if waitingHuman != "" {
				fd.Fields["human"] = waitingHuman
			}
			text, _ := adfWalk(waiting.Body)
			fd.External["comment"] = text
			fd.Suggest = "reply on " + is.Key
			fd.Fingerprint = waiting.ID
			out = append(out, fd)
		}
		if unknown != nil {
			fd := base("unknown-actor", SeverityInfo)
			fd.Since = time.Time(unknown.Created)
			fd.Fields["comment_id"] = unknown.ID
			fd.Fields["account_id"] = unknown.Author.AccountID
			if jiraAccountRe.MatchString(unknown.Author.AccountID) {
				fd.Suggest = "add " + unknown.Author.AccountID + " to assistant.sweep.jira.humans or .bots"
			}
			fd.Fingerprint = unknown.ID
			out = append(out, fd)
		}
		// The persona asked (or is blocked) and no listed human has answered
		// since; bot or unlisted comments in between do not count as answers.
		if involved && !lastPersona.IsZero() && !humanAfter && in.Now.Sub(lastPersona) >= d.chase {
			text := lastPersonaText
			if strings.Contains(text, "?") || strings.EqualFold(status, "Blocked") {
				fd := base("question-unanswered", SeverityActionable)
				fd.Since = lastPersona
				fd.Fields["comment_id"] = lastPersonaID
				fd.External["comment"] = text
				fd.Suggest = "chase " + is.Key
				fd.Fingerprint = lastPersonaID
				out = append(out, fd)
			}
		}
		if assignedToPersona && contains(active, status) {
			switch {
			case f.Status.Category.Key == "new" && len(comments) == 0:
				fd := base("needs-status-update", SeverityInfo)
				fd.Since = time.Time(f.Created)
				fd.Suggest = "note on " + is.Key + " why it waits"
				fd.Fingerprint = "no-comment|" + status
				out = append(out, fd)
			case !transitioned[is.Key]:
				since := lastPersona
				if since.IsZero() {
					since = time.Time(f.Created)
				}
				if in.Now.Sub(since) >= d.statusUpdate {
					fd := base("needs-status-update", SeverityInfo)
					fd.Since = since
					fd.Suggest = "status note on " + is.Key
					fd.Fingerprint = status + "|" + lastPersonaID
					out = append(out, fd)
				}
			}
		}
		if involved && backlog[is.Key] {
			createdByPersona := f.Creator != nil && isPersona[f.Creator.AccountID]
			if contains(active, status) || (createdByPersona && in.Now.Sub(time.Time(f.Created)) <= d.offBoard) {
				fd := base("off-board", SeverityActionable)
				fd.Since = time.Time(f.Created)
				fd.Fields["board"] = fmt.Sprint(cfg.BoardID)
				fd.Suggest = fmt.Sprintf("move %s onto board %d (jira-move-to-board)", is.Key, cfg.BoardID)
				fd.Fingerprint = status
				out = append(out, fd)
			}
		}
	}
	if wip > wipLimit {
		out = append(out, Finding{Kind: "wip-over-limit", Key: "wip", Severity: SeverityInfo, Since: in.Now,
			Fields:  map[string]string{"count": fmt.Sprint(wip), "limit": fmt.Sprint(wipLimit)},
			Suggest: "finish or hand off before starting more", Fingerprint: fmt.Sprint(wip)})
	}
	for _, w := range waves {
		if !jiraKeyRe.MatchString(w.Key) {
			note("release wave with malformed key skipped")
			continue
		}
		if !timeLeft(ctx) {
			note("time budget exhausted before release wave %s", w.Key)
			break
		}
		children, err := c.searchFirst(fmt.Sprintf(`parent = %s ORDER BY updated DESC`, w.Key))
		if err != nil {
			note("%s: subtasks unavailable: %v", w.Key, err)
			continue
		}
		last := time.Time(w.Fields.Updated)
		if len(children) > 0 && time.Time(children[0].Fields.Updated).After(last) {
			last = time.Time(children[0].Fields.Updated)
		}
		if in.Now.Sub(last) >= d.stale {
			fd := jiraBase(w, browse)("release-wave-stale", SeverityInfo)
			fd.Since = last
			fd.Suggest = "review release wave " + w.Key
			fd.Fingerprint = last.UTC().Format(time.RFC3339)
			out = append(out, fd)
		}
	}
	for i := range out {
		if strings.HasPrefix(out[i].Key, "invalid-key-") {
			out[i].Suggest = "" // would quote a malformed key
		}
	}
	if len(partial) > 0 {
		return out, &PartialError{Reason: strings.Join(partial, "; ")}
	}
	return out, nil
}

// jiraBase returns a Finding constructor for an issue. A key outside
// jiraKeyRe never reaches Key, URL or Suggest: the Key becomes a hash
// placeholder, the raw key is fenced, and callers' Suggest (which quotes
// the key) is cleared by the returned constructor's shape flag.
func jiraBase(is jiraIssue, browse string) func(kind, severity string) Finding {
	f := is.Fields
	valid := jiraKeyRe.MatchString(is.Key)
	return func(kind, severity string) Finding {
		fd := Finding{Kind: kind, Key: is.Key, URL: browse + is.Key, Severity: severity,
			Fields:   map[string]string{"status_category": f.Status.Category.Key},
			External: map[string]string{"summary": f.Summary, "status": f.Status.Name}}
		if !valid {
			sum := sha256.Sum256([]byte(is.Key))
			fd.Key, fd.URL = "invalid-key-"+hex.EncodeToString(sum[:6]), ""
			fd.External["key"] = is.Key
		}
		return fd
	}
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if strings.EqualFold(l, s) {
			return true
		}
	}
	return false
}

func mentionsAny(ids []string, want map[string]bool) bool {
	for _, id := range ids {
		if want[id] {
			return true
		}
	}
	return false
}
