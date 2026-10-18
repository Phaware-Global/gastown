package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

var ghNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return ghNow.Add(-d) }

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaM = "cccccccccccccccccccccccccccccccccccccccc"
	shaN = "dddddddddddddddddddddddddddddddddddddddd"
	shaT = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// pr builds an open PR fixture; opts mutate it.
func pr(n int, author string, created, pushed time.Duration, ci string, opts ...func(*ghPR)) ghPR {
	p := ghPR{Number: n, Title: "pr " + author, URL: "https://github.com/acme/app/pull/" + strconv.Itoa(n), CreatedAt: ago(created),
		HeadRefName: "feat/" + strconv.Itoa(n), HeadRefOid: shaA, BaseRefName: "main", Author: ghActor{Login: author}}
	c := ghCommit{CommittedDate: ago(pushed + 48*time.Hour)} // committed long before the push
	c.CheckSuites.Nodes = append(c.CheckSuites.Nodes, struct {
		CreatedAt time.Time `json:"createdAt"`
	}{ago(pushed)})
	if ci != "" {
		c.StatusCheckRollup = &struct {
			State string `json:"state"`
		}{State: ci}
	}
	p.Commits.Nodes = append(p.Commits.Nodes, struct {
		Commit ghCommit `json:"commit"`
	}{c})
	for _, o := range opts {
		o(&p)
	}
	return p
}

func review(login, state, oid string, at time.Duration) func(*ghPR) {
	return func(p *ghPR) {
		r := ghReview{Author: ghActor{Login: login}, State: state, SubmittedAt: ago(at)}
		r.Commit.Oid = oid
		p.Reviews.Nodes = append(p.Reviews.Nodes, r)
	}
}

func requested(login string, at time.Duration) func(*ghPR) {
	return func(p *ghPR) {
		p.ReviewRequests.Nodes = append(p.ReviewRequests.Nodes, ghRequestEvent{RequestedReviewer: ghActor{Login: login}})
		p.TimelineItems.Nodes = append(p.TimelineItems.Nodes, ghRequestEvent{Typename: "ReviewRequestedEvent", CreatedAt: ago(at), RequestedReviewer: ghActor{Login: login}})
	}
}

func thread(login string, at time.Duration, resolved bool) func(*ghPR) {
	return func(p *ghPR) {
		th := ghThread{IsResolved: resolved}
		th.Comments.Nodes = append(th.Comments.Nodes, ghThreadComment{Author: ghActor{Login: login}, CreatedAt: ago(at)})
		p.ReviewThreads.Nodes = append(p.ReviewThreads.Nodes, th)
	}
}

func draft(p *ghPR) { p.IsDraft = true }

func branch(name string) func(*ghPR) { return func(p *ghPR) { p.HeadRefName = name } }

func mergedPR(n int, base, branch string, mergedAgo time.Duration, oid string) ghPR {
	p := ghPR{Number: n, Title: "release pr", URL: "https://github.com/acme/app/pull/" + strconv.Itoa(n), BaseRefName: base, HeadRefName: branch,
		MergedAt: ago(mergedAgo), CreatedAt: ago(mergedAgo + time.Hour), Author: ghActor{Login: "artie"}}
	p.MergeCommit = &struct {
		Oid string `json:"oid"`
	}{Oid: oid}
	return p
}

func tagAt(oid string, at time.Duration) ghTag {
	t := ghTag{Name: "v"}
	t.Target.Oid, t.Target.CommittedDate = oid, ago(at)
	return t
}

// withJira wires a Jira port at the same test server and the link settings.
func withJira(p *GitHubPort, in Input, srvURL string) {
	p.jira = &JiraPort{
		persona: func() ([]string, error) { return []string{personaID}, nil },
		getenv:  func(string) string { return "tok" },
		client:  p.client,
		base:    func(string) string { return srvURL },
	}
	in.Settings.Assistant.Sweep.Jira = &config.JiraSweepConfig{Site: "example.atlassian.net", Email: "persona@example.com", Projects: []string{"P23"},
		Reviewers: map[string]string{"kevin": humanID}}
}

type ghFixture struct {
	calls       atomic.Int32
	repos       map[string]*ghRepoData // "owner/name"
	failing     string                 // repo slug whose GraphQL call errors
	owned       map[int]bool           // patrol-owned PR numbers
	slow        time.Duration          // delay every GraphQL answer
	paged       map[int]*ghPRPage      // full reviews/threads served to the per-PR paging query
	pages       map[int][]*ghPRPage    // multi-page answers, selected by the rcur cursor "p<n>"
	pageErr     int                    // 1-based page index that answers with errors and null connections (0 = none)
	pageErrMode string                 // "both" (default): errors + null connections; "errors": errors with valid data; "null": null connections, no errors
	fieldErrPR  int                    // repo query: this PR's reviews come back null and an errors entry is attached
	nullNodePR  int                    // repo query: this PR's whole node comes back null (NON_NULL field denied)
	pageQueries []int                  // PR numbers the per-PR paging query was asked for
	patrolCalls atomic.Int32
	diffs       map[string]string           // "base...sha" → unified diff text; unlisted specs differ from each other
	mergeBases  map[string]string           // "base...sha" → merge base sha (default "mb0": the base never moved)
	baseFiles   map[string][]string         // "mbOld...mbNew" → files the base touched between the merge bases
	compareFail []string                    // compare specs that answer 500
	patrolDelay time.Duration               // patrolBeads sleeps this long
	patches     map[string][]map[string]any // "base...sha" → compare files; unlisted specs differ from each other
	compare     map[string]string           // "base...head" → status
	issues      []map[string]any            // Jira issues returned for `key in (...)`
	jiraErr     bool                        // Jira search fails
	jiraBadKey  string                      // a `key in (...)` batch naming this key answers 400
}

func (fx *ghFixture) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.calls.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/rest/") && r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing token on %s", r.URL)
		}
		switch {
		case r.URL.Path == "/rest/api/3/search/jql":
			if fx.jiraErr {
				http.Error(w, "jira down", 500)
				return
			}
			jql := r.URL.Query().Get("jql")
			if fx.jiraBadKey != "" && strings.Contains(jql, fx.jiraBadKey) {
				http.Error(w, `{"errorMessages":["An issue with key '`+fx.jiraBadKey+`' does not exist for field 'key'."]}`, 400)
				return
			}
			var hit []map[string]any
			for _, is := range fx.issues {
				k := is["key"].(string)
				if strings.Contains(jql, `"`+k+`"`) || strings.HasPrefix(jql, "key = "+k+" ") || strings.HasSuffix(jql, "key = "+k) {
					hit = append(hit, is)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": hit, "isLast": true})
		case strings.Contains(r.URL.Path, "/compare/"):
			spec := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for _, f := range fx.compareFail {
				if f == spec {
					http.Error(w, "compare unavailable", 500)
					return
				}
			}
			if r.Header.Get("Accept") == "application/vnd.github.diff" {
				d, ok := fx.diffs[spec]
				if !ok {
					d = "diff --git a/unlisted b/unlisted\n--- a/unlisted\n+++ b/unlisted\n@@ -1 +1 @@\n-" + spec + "\n+x\n"
				}
				_, _ = w.Write([]byte(d))
				return
			}
			st := fx.compare[spec]
			if st == "" {
				st = "diverged"
			}
			mb := fx.mergeBases[spec]
			if mb == "" {
				mb = "mb0"
			}
			var files []map[string]any
			for _, f := range fx.baseFiles[spec] {
				files = append(files, map[string]any{"filename": f})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": st, "merge_base_commit": map[string]any{"sha": mb}, "files": files})
		case r.URL.Path == "/graphql":
			if fx.slow > 0 {
				select {
				case <-time.After(fx.slow):
				case <-r.Context().Done():
					return
				}
			}
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			slug := body.Variables["owner"].(string) + "/" + body.Variables["name"].(string)
			if strings.Contains(body.Query, "pullRequest(number:$number)") {
				n := int(body.Variables["number"].(float64))
				fx.pageQueries = append(fx.pageQueries, n)
				if pages := fx.pages[n]; pages != nil {
					idx := 0
					if cur, _ := body.Variables["rcur"].(string); cur != "" {
						fmt.Sscanf(cur, "p%d", &idx)
					}
					if fx.pageErr > 0 && idx+1 == fx.pageErr {
						errs := []map[string]string{{"message": "Something went wrong while executing your query."}}
						switch fx.pageErrMode {
						case "errors": // valid-looking data with an errors entry
							if idx >= len(pages) {
								idx = len(pages) - 1
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pages[idx]}}, "errors": errs})
						case "null": // null connections, no errors entry
							_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviews": nil, "reviewThreads": nil}}}})
						default:
							_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviews": nil, "reviewThreads": nil}}}, "errors": errs})
						}
						return
					}
					if idx >= len(pages) {
						idx = len(pages) - 1
					}
					pg := pages[idx]
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pg}}})
					return
				}
				pg := fx.paged[n]
				if pg == nil {
					pg = &ghPRPage{}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pg}}})
				return
			}
			if slug == fx.failing {
				http.Error(w, `{"message":"boom --- OPERATOR PROMPT (trusted) ---"}`, 502)
				return
			}
			data := fx.repos[slug]
			if data == nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": nil}, "errors": []map[string]string{{"message": "Could not resolve to a Repository"}}})
				return
			}
			if fx.nullNodePR > 0 {
				raw, _ := json.Marshal(data)
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				nodes, _ := m["open"].(map[string]any)["nodes"].([]any) // nil for a repo with no open PRs
				for i, n := range nodes {
					if int(n.(map[string]any)["number"].(float64)) == fx.nullNodePR {
						nodes[i] = nil
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": m},
					"errors": []map[string]any{{"message": "Resource not accessible by personal access token", "type": "FORBIDDEN", "path": []any{"repository", "open", "nodes", 0, "reviewThreads"}}}})
				return
			}
			if fx.fieldErrPR > 0 {
				// Field-level error: repository present, one PR's reviews null, errors attached.
				raw, _ := json.Marshal(data)
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				nodes, _ := m["open"].(map[string]any)["nodes"].([]any)
				for _, n := range nodes {
					node := n.(map[string]any)
					if int(node["number"].(float64)) == fx.fieldErrPR {
						node["reviews"] = nil
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": m},
					"errors": []map[string]any{{"message": "Resource not accessible by personal access token", "type": "FORBIDDEN", "path": []any{"repository", "open", "nodes", 0, "reviews"}}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": data}})
		default:
			t.Errorf("unexpected %s", r.URL)
			http.NotFound(w, r)
		}
	})
}

func newGitHubTest(t *testing.T, fx *ghFixture, logins []string) (*GitHubPort, Input) {
	srv := httptest.NewServer(fx.handler(t))
	t.Cleanup(srv.Close)
	p := &GitHubPort{
		persona: func() ([]string, []string, error) { return logins, []string{"acme/extra"}, nil },
		repos: func() ([]repoRef, error) {
			return []repoRef{{Owner: "acme", Name: "app", Rig: "app", Reviewer: "phaware-val", Approver: "kevin"}}, nil
		},
		token: func(string) string { return "tok" },
		patrolBeads: func(rig string) (map[int]bool, error) {
			fx.patrolCalls.Add(1)
			if fx.patrolDelay > 0 {
				time.Sleep(fx.patrolDelay)
			}
			return fx.owned, nil
		},
		client:  srv.Client(),
		apiBase: srv.URL,
	}
	settings := &config.TownSettings{Assistant: &config.AssistantConfig{Sweep: &config.SweepConfig{GitHub: &config.GitHubSweepConfig{Humans: []string{"kevin"}}}}}
	return p, Input{Now: ghNow, Settings: settings}
}

// The rig's configured approver is human even when assistant.sweep.github
// .humans is empty: their approval and their pending request must be seen,
// or every reviewer-approved PR reads as "ready for human review" forever.
func TestGitHubPort_ApproverIsHumanWithoutHumansList(t *testing.T) {
	h := time.Hour
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{
		pr(1, "artie", 5*h, 5*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 4*h), review("kevin", "APPROVED", shaA, 3*h)),
		pr(2, "artie", 40*h, 40*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 39*h), requested("kevin", 30*h)),
	}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	in.Settings.Assistant.Sweep.GitHub.Humans = nil
	findings, err := p.Sweep(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = true
	}
	for _, absent := range []string{"ready-for-human-review/acme/app#1", "ready-for-human-review/acme/app#2", "unknown-actor/acme/app#1", "unknown-actor/acme/app#2"} {
		if got[absent] {
			t.Errorf("unexpected %s; got %v", absent, got)
		}
	}
	if !got["review-waiting/acme/app#2@kevin"] {
		t.Errorf("the approver's pending request must be review-waiting; got %v", got)
	}
}

// A stale approval tells a base-branch merge (the PR's own diff is
// unchanged: no new review needed) from a material change (ask again),
// including a mode change, a rename, a binary swap or a moved hunk — even a
// hunk moved within one function context, since line numbers are kept
// unless the base itself touched the file between the two merge bases. A
// compare that cannot answer (error or spent budget) is "unknown", asks
// again, fences the reason, and never makes the port partial. The
// fingerprint is the pair of heads, so a transient compare error never
// re-pages an acked item.
func TestGitHubPort_ApprovalStaleDiff(t *testing.T) {
	h := time.Hour
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }
	head := func(sha string) func(*ghPR) { return func(p *ghPR) { p.HeadRefOid = sha } }
	stale := func(n int, headSHA string) ghPR {
		return pr(n, "artie", 50*h, 2*h, "SUCCESS", review("phaware-val", "APPROVED", headSHA, 1*h), review("kevin", "APPROVED", shaB, 20*h), head(headSHA))
	}
	const approved = "diff --git a/a.go b/a.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@ func f()\n-x\n+y\n"
	const approvedBinary = "diff --git a/x.bin b/x.bin\nindex 1111111..aaaaaaa 100644\nBinary files a/x.bin and b/x.bin differ\n"
	diffs := map[string]string{
		"main..." + shaB:     approved,
		"main..." + sha('a'): "diff --git a/a.go b/a.go\nindex 3333333..4444444 100644\n--- a/a.go\n+++ b/a.go\n@@ -7,3 +7,3 @@ func f()\n-x\n+y\n",                                                         // base moved a.go: blob ids and ranges shift equally
		"main..." + sha('c'): strings.Replace(approved, "+y", "+z", 1),                                                                                                                                      // material change
		"main..." + sha('e'): "diff --git a/a.go b/a.go\nold mode 100644\nnew mode 100755\nindex 1111111..2222222\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@ func f()\n-x\n+y\n",                              // chmod +x
		"main..." + sha('f'): "diff --git a/a.go b/b.go\nsimilarity index 90%\nrename from a.go\nrename to b.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/b.go\n@@ -1,3 +1,3 @@ func f()\n-x\n+y\n", // rename
		"main..." + sha('g'): "diff --git a/a.go b/a.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/a.go\n@@ -40,3 +40,3 @@ func g()\n-x\n+y\n",                                                       // same hunk body moved into another function
		"main..." + sha('h'): "diff --git a/x.bin b/x.bin\nindex 1111111..bbbbbbb 100644\nBinary files a/x.bin and b/x.bin differ\n",                                                                        // binary swapped after approval
		"main..." + sha('i'): "diff --git a/x.bin b/x.bin\nindex 3333333..aaaaaaa 100644\nBinary files a/x.bin and b/x.bin differ\n",                                                                        // same head blob, base moved
		"main..." + sha('j'): "diff --git a/a.go b/a.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/a.go\n@@ -40,3 +40,3 @@ func f()\n-x\n+y\n",                                                       // same body and context, moved within the function; base did not touch a.go
	}
	mergeBases := map[string]string{"main..." + sha('a'): "mb1", "main..." + sha('i'): "mb1"}
	baseFiles := map[string][]string{"mb0...mb1": {"a.go", "x.bin"}}
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{stale(1, sha('a')), stale(2, sha('c')), stale(3, sha('d')), stale(4, sha('e')), stale(5, sha('f')), stale(6, sha('g')), stale(9, sha('j'))}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, diffs: diffs, mergeBases: mergeBases, baseFiles: baseFiles, compareFail: []string{"main..." + sha('d')}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	findings, err := p.Sweep(context.Background(), in)
	if err != nil {
		t.Fatalf("a failed compare must not make the port partial: %v", err)
	}
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	for _, c := range []struct{ key, diff, suggest, fp string }{
		{"acme/app#1", "unchanged", "acme/app#1 only merged its base branch since kevin approved: no new review needed", sha('a') + "|" + shaB},
		{"acme/app#2", "changed", "ask kevin to re-approve acme/app#2", sha('c') + "|" + shaB},
		{"acme/app#3", "unknown", "ask kevin to re-approve acme/app#3", sha('d') + "|" + shaB},
		{"acme/app#4", "changed", "ask kevin to re-approve acme/app#4", sha('e') + "|" + shaB},
		{"acme/app#5", "changed", "ask kevin to re-approve acme/app#5", sha('f') + "|" + shaB},
		{"acme/app#6", "changed", "ask kevin to re-approve acme/app#6", sha('g') + "|" + shaB},
		{"acme/app#9", "changed", "ask kevin to re-approve acme/app#9", sha('j') + "|" + shaB},
	} {
		f, ok := got["approval-stale/"+c.key]
		if !ok || f.Fields["diff"] != c.diff || f.Suggest != c.suggest || f.Fingerprint != c.fp {
			t.Errorf("%s: want diff=%s suggest=%q fp=%s, got ok=%v %+v", c.key, c.diff, c.suggest, c.fp, ok, f)
		}
	}
	if e := got["approval-stale/acme/app#3"].External["diff_error"]; !strings.Contains(e, "500") {
		t.Errorf("the compare error must be fenced in diff_error, got %q", e)
	}
	// Decoy paths containing " b/": a text decoy is keyed on its own "+++ b/"
	// path, so a change to the real internal/auth.go beside it still reads
	// "changed"; a binary decoy that aliases a real entry's key makes the
	// verdict "unknown" rather than hiding the real change.
	const decoyApproved = "diff --git a/zz b/internal/auth.go b/zz b/internal/auth.go\nindex 1111111..2222222 100644\n--- a/zz b/internal/auth.go\n+++ b/zz b/internal/auth.go\n@@ -1 +1 @@ decoy\n-d\n+e\ndiff --git a/internal/auth.go b/internal/auth.go\nindex 1111111..2222222 100644\n--- a/internal/auth.go\n+++ b/internal/auth.go\n@@ -1 +1 @@ func auth()\n-x\n+y\n"
	const decoyBinaryApproved = "diff --git a/zz b/x.bin b/zz b/x.bin\nindex 1111111..2222222 100644\nBinary files a/zz b/x.bin and b/zz b/x.bin differ\ndiff --git a/x.bin b/x.bin\nindex 1111111..aaaaaaa 100644\nBinary files a/x.bin and b/x.bin differ\n"
	app.Open.Nodes = []ghPR{stale(10, sha('k')), stale(11, sha('l'))}
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, diffs: map[string]string{
		"main..." + shaB:     decoyApproved,
		"main..." + sha('k'): strings.Replace(decoyApproved, "+y", "+z", 1), // the real file changed after approval
		"main..." + sha('l'): decoyBinaryApproved,                           // aliased keys
	}}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	findings, _ = p.Sweep(context.Background(), in)
	got = map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	if got["approval-stale/acme/app#10"].Fields["diff"] != "changed" {
		t.Errorf("a text decoy must not hide a change to the real file: %v", got["approval-stale/acme/app#10"].Fields)
	}
	if f := got["approval-stale/acme/app#11"]; f.Fields["diff"] != "unknown" || !strings.Contains(f.External["diff_error"], "ambiguous diff path") {
		t.Errorf("aliased keys must be unknown with the reason fenced: %v / %v", f.Fields, f.External)
	}
	// Git writes "+++ b/foo bar.go\t" for a spaced path: the key drops the
	// tab, so a base-only move of that file still reads "unchanged"; a
	// file-to-symlink typechange (two sections under one header) is not a
	// duplicate key.
	const spacedApproved = "diff --git a/foo bar.go b/foo bar.go\nindex 1111111..2222222 100644\n--- a/foo bar.go\t\n+++ b/foo bar.go\t\n@@ -1,3 +1,3 @@ func f()\n-x\n+y\n"
	const typechange = "diff --git a/t b/t\ndeleted file mode 100644\nindex 1111111..0000000\n--- a/t\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\ndiff --git a/t b/t\nnew file mode 120000\nindex 0000000..2222222\n--- /dev/null\n+++ b/t\n@@ -0,0 +1 @@\n+target\n"
	// Binary and empty files turned into symlinks have a deletion half with
	// no "---"/"+++" lines; the "deleted file mode" header keys it apart, so
	// an identical diff at both heads is "unchanged".
	const binTypechange = "diff --git a/b.bin b/b.bin\ndeleted file mode 100644\nindex 1111111..0000000\nBinary files a/b.bin and /dev/null differ\ndiff --git a/b.bin b/b.bin\nnew file mode 120000\nindex 0000000..2222222\n--- /dev/null\n+++ b/b.bin\n@@ -0,0 +1 @@\n+target\n"
	const emptyTypechange = "diff --git a/e b/e\ndeleted file mode 100644\nindex e69de29..0000000\ndiff --git a/e b/e\nnew file mode 120000\nindex 0000000..2222222\n--- /dev/null\n+++ b/e\n@@ -0,0 +1 @@\n+target\n"
	app.Open.Nodes = []ghPR{stale(12, sha('m')), stale(13, sha('n')), stale(14, sha('o'))}
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}},
		diffs: map[string]string{
			"main..." + shaB:     spacedApproved + typechange + binTypechange + emptyTypechange,
			"main..." + sha('m'): strings.Replace(spacedApproved, "@@ -1,3 +1,3 @@", "@@ -9,3 +9,3 @@", 1) + typechange + binTypechange + emptyTypechange, // base moved "foo bar.go"
			"main..." + sha('n'): spacedApproved + strings.Replace(typechange, "+target", "+elsewhere", 1) + binTypechange + emptyTypechange,              // symlink target changed
			"main..." + sha('o'): spacedApproved + typechange + binTypechange + emptyTypechange,                                                           // byte-identical
		},
		mergeBases: map[string]string{"main..." + sha('m'): "mb1", "main..." + sha('n'): "mb1"},
		baseFiles:  map[string][]string{"mb0...mb1": {"foo bar.go"}}}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	findings, _ = p.Sweep(context.Background(), in)
	got = map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	if got["approval-stale/acme/app#12"].Fields["diff"] != "unchanged" || got["approval-stale/acme/app#13"].Fields["diff"] != "changed" || got["approval-stale/acme/app#14"].Fields["diff"] != "unchanged" {
		t.Errorf("spaced path / typechanges: want #12 unchanged, #13 changed, #14 unchanged; got %v / %v / %v (%v)", got["approval-stale/acme/app#12"].Fields, got["approval-stale/acme/app#13"].Fields, got["approval-stale/acme/app#14"].Fields, got["approval-stale/acme/app#14"].External["diff_error"])
	}
	// Binary entries: the approved head added x.bin; a swapped binary is a
	// change even though the diff has no hunk body, while a base merge that
	// only moved the base-side blob is not.
	app.Open.Nodes = []ghPR{stale(7, sha('h')), stale(8, sha('i'))}
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, diffs: map[string]string{"main..." + shaB: approvedBinary, "main..." + sha('h'): diffs["main..."+sha('h')], "main..." + sha('i'): diffs["main..."+sha('i')]}, mergeBases: mergeBases, baseFiles: baseFiles}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	findings, _ = p.Sweep(context.Background(), in)
	got = map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	if got["approval-stale/acme/app#7"].Fields["diff"] != "changed" || got["approval-stale/acme/app#8"].Fields["diff"] != "unchanged" {
		t.Errorf("binary: want #7 changed (swapped blob) and #8 unchanged (same head blob), got %v / %v", got["approval-stale/acme/app#7"].Fields, got["approval-stale/acme/app#8"].Fields)
	}
	// Three sweeps of one merge-only PR: unchanged, then a compare 500, then
	// unchanged again — same fingerprint throughout, so an acked item never
	// re-pages on a hiccup.
	app.Open.Nodes = []ghPR{stale(1, sha('a'))}
	var fps []string
	for i, fail := range [][]string{nil, {"main..." + sha('a')}, nil} {
		fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, diffs: diffs, mergeBases: mergeBases, baseFiles: baseFiles, compareFail: fail}
		p, in = newGitHubTest(t, fx, []string{"artie"})
		findings, err = p.Sweep(context.Background(), in)
		if err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
		for _, f := range findings {
			if f.Kind == "approval-stale" {
				fps = append(fps, f.Fingerprint+"/"+f.Fields["diff"])
			}
		}
	}
	want := []string{sha('a') + "|" + shaB + "/unchanged", sha('a') + "|" + shaB + "/unknown", sha('a') + "|" + shaB + "/unchanged"}
	if !slices.Equal(fps, want) {
		t.Errorf("fingerprint must survive a transient compare error: got %v", fps)
	}
	// Many stale approvals: the per-sweep compare budget covers some; the
	// rest are "unknown" with re-approve advice and no partial note.
	app.Open.Nodes = nil
	for n := 1; n <= 11; n++ {
		app.Open.Nodes = append(app.Open.Nodes, stale(n, sha('c')))
	}
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, diffs: diffs}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	findings, err = p.Sweep(context.Background(), in)
	if err != nil {
		t.Errorf("a spent compare budget must not make the port partial: %v", err)
	}
	counts := map[string]int{}
	for _, f := range findings {
		if f.Kind == "approval-stale" {
			counts[f.Fields["diff"]]++
		}
	}
	covered := (githubComparesPerSweep + 3) / 4 // a PR starts while calls so far < budget, four calls each (same merge base)
	if counts["changed"] != covered || counts["unknown"] != 11-covered {
		t.Errorf("budget: want %d changed + %d unknown, got %v", covered, 11-covered, counts)
	}
}

func TestGitHubPort_Findings(t *testing.T) {
	h := time.Hour
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{
		pr(1, "artie", 10*h, 5*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 4*h)),
		pr(2, "artie", 40*h, 40*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 39*h), requested("kevin", 30*h)),
		pr(3, "artie", 50*h, 2*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 1*h), review("kevin", "APPROVED", shaB, 20*h)),
		pr(4, "artie", 50*h, 10*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 9*h), review("kevin", "APPROVED", shaA, 5*h)),
		pr(5, "artie", 50*h, 20*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 19*h), review("kevin", "CHANGES_REQUESTED", shaA, 13*h), thread("kevin", 13*h, false)),
		pr(6, "artie", 3*h, 3*h, "PENDING"),
		pr(7, "someone", 30*h, 2*h, "FAILURE"),
		pr(8, "artie", 30*h, 2*h, "FAILURE", review("phaware-val", "APPROVED", shaA, 1*h)),
		pr(9, "artie", 200*h, 200*h, "", draft),
		pr(10, "artie", 2*h, 2*h, "", draft),
		pr(11, "artie", 5*h, 5*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 4*h), requested("stranger", 4*h)),
	}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, owned: map[int]bool{8: true}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	findings, err := p.Sweep(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	for _, want := range []string{"ready-for-human-review/acme/app#1", "review-waiting/acme/app#2@kevin", "approval-stale/acme/app#3", "approved-unmerged/acme/app#4",
		"changes-requested-stale/acme/app#5", "no-review-loop/acme/app#6", "ci-failing/acme/app#7", "draft-stale/acme/app#9", "unknown-actor/acme/app#11"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s; got %v", want, keysOf(got))
		}
	}
	for _, absent := range []string{"ci-failing/acme/app#8", "draft-stale/acme/app#10", "ready-for-human-review/acme/app#2", "no-review-loop/acme/app#7"} {
		if _, ok := got[absent]; ok {
			t.Errorf("unexpected %s", absent)
		}
	}
	if f := got["review-waiting/acme/app#2@kevin"]; f.Fields["human"] != "kevin" || f.Fingerprint != "kevin|"+ago(30*time.Hour).UTC().Format(time.RFC3339)+"|1" || !strings.Contains(f.Suggest, "on acme/app#2@kevin") {
		t.Errorf("review-waiting: %+v", f)
	}
	if f := got["ready-for-human-review/acme/app#1"]; f.Fields["approver"] != "kevin" || !strings.Contains(f.Suggest, "kevin") {
		t.Errorf("ready: %+v", f)
	}
	if f := got["unknown-actor/acme/app#11"]; f.External["logins"] != "stranger" || strings.Contains(f.Suggest, "stranger") {
		t.Errorf("stranger login must stay External: %+v", f)
	}
	for k, f := range got {
		for _, ext := range []string{"title", "branch", "author"} {
			if _, inFields := f.Fields[ext]; inFields {
				t.Errorf("%s: %s must be External", k, ext)
			}
		}
		before := f.Suggest
		enforceSchema(p.Schema(), p.Suggestions(), &f)
		if f.Suggest != before {
			t.Errorf("%s: suggest %q rejected by the templates", k, before)
		}
	}
}

func TestGitHubPort_PartialDryRunAndErrors(t *testing.T) {
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{pr(6, "artie", 3*time.Hour, 3*time.Hour, "PENDING")}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app}, failing: "acme/extra"}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	findings, err := p.Sweep(context.Background(), in)
	var partial *PartialError
	if !errors.As(err, &partial) || len(findings) != 1 || !strings.Contains(partial.Reason, "acme/extra") {
		t.Fatalf("a failing repo must make the port partial, keeping the rest: %v / %v", err, findings)
	}

	in.DryRun = true
	var traced []string
	in.Trace = func(q string) { traced = append(traced, q) }
	before := fx.calls.Load()
	if f, err := p.Sweep(context.Background(), in); err != nil || len(f) != 0 || fx.calls.Load() != before {
		t.Fatalf("dry run must not call GitHub: %v %v", err, f)
	}
	if !strings.Contains(strings.Join(traced, "\n"), "repository(acme/app)") {
		t.Errorf("dry run should trace the repo query: %v", traced)
	}

	p, in = newGitHubTest(t, &ghFixture{}, nil)
	if _, err := p.Sweep(context.Background(), in); err == nil || !strings.Contains(err.Error(), "github_logins") {
		t.Errorf("missing persona login must fail: %v", err)
	}
	p, in = newGitHubTest(t, &ghFixture{}, []string{"artie"})
	p.token = func(string) string { return "" }
	if _, err := p.Sweep(context.Background(), in); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("missing token must fail: %v", err)
	}
}

func TestGitHubOwnerRepo(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/Phaware-Global/gastown":     "Phaware-Global/gastown",
		"https://github.com/Phaware-Global/gastown.git": "Phaware-Global/gastown",
		"git@github.com:acme/app.git":                   "acme/app",
		"https://gitlab.com/acme/app":                   "",
	} {
		o, r, ok := githubOwnerRepo(raw)
		if got := o + "/" + r; (ok && got != want) || (!ok && want != "") {
			t.Errorf("%s → %q ok=%v, want %q", raw, got, ok, want)
		}
	}
}

func TestGitHubPort_ReleasesAndLinkedIssues(t *testing.T) {
	h := time.Hour
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{
		pr(30, "artie", 40*h, 40*h, "SUCCESS", branch("feat/P23-30-thing"), requested("kevin", 30*h)),
		pr(31, "artie", 40*h, 40*h, "SUCCESS", branch("feat/p23-31-lower"), requested("kevin", 30*h)),
		pr(32, "artie", 40*h, 20*h, "SUCCESS", branch("feat/P23-32"), review("kevin", "CHANGES_REQUESTED", shaA, 2*h)),
		pr(33, "artie", 40*h, 40*h, "SUCCESS", branch("feat/P23-33"), requested("kevin", 30*h), draft),
	}
	app.Merged.Nodes = []ghPR{
		mergedPR(40, "release/1.4", "feat/P23-40", 48*h, shaM),
		mergedPR(41, "release/1.4", "feat/P23-41", 48*h, shaN),
		mergedPR(42, "main", "feat/P23-42", 48*h, shaB),
		mergedPR(43, "release/1.5", "feat/P23-43", 2*h, shaT),
	}
	app.Tags.Nodes = []ghTag{tagAt(shaB, 100*h), tagAt(shaT, 30*h)}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}},
		compare: map[string]string{shaM + "..." + shaT: "ahead"}, // v tag contains #40's merge
		issues: []map[string]any{
			issue("P23-30", "In Progress", "indeterminate", personaID, 100*h, 1*h, "needs handoff"),
			issue("P23-31", "In Review", "indeterminate", humanID, 100*h, 1*h, "handed off correctly"),
			issue("P23-32", "In Review", "indeterminate", humanID, 100*h, 1*h, "handed back"),
			issue("P23-33", "To Do", "new", personaID, 100*h, 1*h, "draft"),
			issue("P23-40", "Done", "done", personaID, 100*h, 1*h, "done, merged, tagged, not in UAT"),
			issue("P23-41", "In UAT", "indeterminate", personaID, 100*h, 1*h, "already in UAT"),
			issue("P23-42", "Done", "done", personaID, 100*h, 1*h, "merged to main"),
			issue("P23-43", "Done", "done", personaID, 100*h, 1*h, "merged too recently for the UAT check"),
		}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	withJira(p, in, p.apiBase)
	findings, err := p.Sweep(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	for _, want := range []string{"review-handoff/P23-30", "review-handoff/P23-32", "done-not-in-uat/P23-40", "release-unclosed/acme/app#41"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s; got %v", want, keysOf(got))
		}
	}
	for _, absent := range []string{"review-handoff/P23-31", "review-handoff/P23-33", "done-not-in-uat/P23-41", "done-not-in-uat/P23-42", "done-not-in-uat/P23-43",
		"release-unclosed/acme/app#40", "release-unclosed/acme/app#42", "release-unclosed/acme/app#43"} {
		if _, ok := got[absent]; ok {
			t.Errorf("unexpected %s", absent)
		}
	}
	if f := got["review-handoff/P23-30"]; f.Fields["human"] != "kevin" || f.Fields["account_id"] != humanID || f.Fields["pr"] != "30" || !strings.Contains(f.Suggest, "review status") {
		t.Errorf("handoff: %+v", f)
	}
	if f := got["review-handoff/P23-32"]; !strings.Contains(f.Suggest, "back to in progress") {
		t.Errorf("hand-back: %+v", f)
	}
	for k, f := range got {
		if _, ok := f.Fields["status"]; ok {
			t.Errorf("%s: status name must be External", k)
		}
		before := f.Suggest
		enforceSchema(p.Schema(), p.Suggestions(), &f)
		if f.Suggest != before {
			t.Errorf("%s: suggest %q rejected by the templates", k, before)
		}
	}

	// Jira down: PR-linked kinds are skipped with a partial note; the rest stands.
	fx.jiraErr = true
	findings, err = p.Sweep(context.Background(), in)
	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "could not be looked up") {
		t.Fatalf("Jira failure must be partial: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "review-handoff" || f.Kind == "done-not-in-uat" {
			t.Errorf("linked kind reported without Jira data: %+v", f)
		}
		if f.Kind == "release-unclosed" {
			return
		}
	}
	t.Error("release-unclosed must still be reported when Jira is down")
}
func forcePushed(at time.Duration) func(*ghPR) {
	return func(p *ghPR) {
		p.Commits.Nodes[0].Commit.CheckSuites.Nodes = nil
		p.ForcePushes.Nodes = append(p.ForcePushes.Nodes, ghRequestEvent{Typename: "HeadRefForcePushedEvent", CreatedAt: ago(at)})
	}
}

// committedAfterForcePush: no check suite, an old force-push and a newer
// normal commit (status-API-only CI).
func committedAfterForcePush(forceAt, commitAt time.Duration) func(*ghPR) {
	return func(p *ghPR) {
		p.Commits.Nodes[0].Commit.CheckSuites.Nodes = nil
		p.Commits.Nodes[0].Commit.CommittedDate = ago(commitAt)
		p.ForcePushes.Nodes = append(p.ForcePushes.Nodes, ghRequestEvent{Typename: "HeadRefForcePushedEvent", CreatedAt: ago(forceAt)})
	}
}

func truncatedReviews(p *ghPR) { p.Reviews.PageInfo.HasPreviousPage = true }

func TestGitHubPort_ReviewRound1Fixes(t *testing.T) {
	h := time.Hour
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{
		// 50: two humans requested changes; the earliest one and the sorted set drive the finding, whatever the map order.
		pr(50, "artie", 60*h, 40*h, "SUCCESS", review("phaware-val", "APPROVED", shaA, 39*h), review("Kevin", "CHANGES_REQUESTED", shaA, 20*h), review("ada", "CHANGES_REQUESTED", shaA, 30*h)),
		// 51: Kevin reviewed this head 20h ago, then was re-requested 10h ago → waiting again (not suppressed).
		pr(51, "artie", 60*h, 40*h, "SUCCESS", review("kevin", "CHANGES_REQUESTED", shaA, 20*h), requested("kevin", 10*h)),
		// 52: reviews window overflowed → paged; kevin's CHANGES_REQUESTED lives on the page.
		pr(52, "artie", 60*h, 40*h, "SUCCESS", truncatedReviews),
		// 55: no check suite, force-pushed 50h ago but a normal commit 10m ago → too fresh to report.
		pr(55, "someone", 60*h, 1*h, "FAILURE", committedAfterForcePush(50*h, 10*time.Minute)),
		// 56: a stranger reviewed twice → one login in the unknown-actor fingerprint.
		pr(56, "artie", 60*h, 40*h, "SUCCESS", review("bob", "APPROVED", shaA, 30*h), review("bob", "CHANGES_REQUESTED", shaA, 20*h)),
		// 53: no check suite; committed 48h+ ago but force-pushed 10 minutes ago → CI failure is too fresh to report.
		pr(53, "someone", 60*h, 1*h, "FAILURE", forcePushed(10*time.Minute)),
		// 54: the persona approved its own PR while also listed as a human → not a human approval.
		pr(54, "artie", 60*h, 10*h, "SUCCESS", review("artie", "APPROVED", shaA, 5*h)),
	}
	paged := &ghPRPage{}
	hidden := ghReview{Author: ghActor{Login: "kevin"}, State: "CHANGES_REQUESTED", SubmittedAt: ago(20 * h)}
	hidden.Commit.Oid = shaA
	paged.Reviews.Nodes = []ghReview{hidden}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, paged: map[int]*ghPRPage{52: paged}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	in.Settings.Assistant.Sweep.GitHub.Humans = []string{"KEVIN", "ada", "artie"} // mis-cased and persona-polluted on purpose
	in.Settings.Assistant.Sweep.GitHub.RemindAfter = "1h"
	run := func() (map[string]Finding, error) {
		fs, err := p.Sweep(context.Background(), in)
		got := map[string]Finding{}
		for _, f := range fs {
			got[f.Kind+"/"+f.Key] = f
		}
		return got, err
	}
	got, err := run()
	if err != nil {
		t.Fatalf("an overflowed review window is paged, not declared partial: %v", err)
	}
	if _, ok := got["changes-requested-stale/acme/app#52"]; !ok {
		t.Errorf("the review hidden behind the window must be found by paging: %v", keysOf(got))
	}
	f := got["changes-requested-stale/acme/app#50"]
	if f.Fields["human"] != "ada" || !strings.HasSuffix(f.Fingerprint, "|ada,kevin") {
		t.Errorf("earliest requester and sorted login set expected: %+v", f)
	}
	for i := 0; i < 5; i++ { // deterministic across runs
		again, _ := run()
		if again["changes-requested-stale/acme/app#50"].Fingerprint != f.Fingerprint {
			t.Fatalf("fingerprint flipped between runs")
		}
	}
	if _, ok := got["review-waiting/acme/app#51@kevin"]; !ok {
		t.Errorf("a re-request after a review on the same head must wait again: %v", keysOf(got))
	}
	if _, ok := got["ci-failing/acme/app#53"]; ok {
		t.Error("a force-push 10 minutes ago must not count as an hour-old failure")
	}
	if _, ok := got["approved-unmerged/acme/app#54"]; ok {
		t.Error("the persona's own approval must never count as the human gate")
	}
	if _, ok := got["unknown-actor/acme/app#50"]; ok {
		t.Error("mis-cased listed humans must still be recognised")
	}
	if _, ok := got["ci-failing/acme/app#55"]; ok {
		t.Error("a normal commit newer than an old force-push must count as the push time")
	}
	if f := got["unknown-actor/acme/app#56"]; f.Fingerprint != "bob" {
		t.Errorf("strangers must be deduped per login: %q", f.Fingerprint)
	}
}

func TestGitHubPort_HungPatrolLookupIsPartialNotTimeout(t *testing.T) {
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{pr(7, "someone", 30*time.Hour, 2*time.Hour, "FAILURE"), pr(6, "artie", 3*time.Hour, 3*time.Hour, "PENDING")}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, patrolDelay: 3 * time.Second}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	oldMargin, oldTimeout := budgetMargin, PortTimeout
	budgetMargin, PortTimeout = 200*time.Millisecond, 1200*time.Millisecond
	defer func() { budgetMargin, PortTimeout = oldMargin, oldTimeout }()
	findings, err := runPort(context.Background(), p, in)
	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "patrol beads unavailable") {
		t.Fatalf("a hung bd must become a partial note, got %v", err)
	}
	keys := map[string]bool{}
	for _, f := range findings {
		keys[f.Kind+"/"+f.Key] = true
	}
	if !keys["ci-failing/acme/app#7"] || !keys["no-review-loop/acme/app#6"] {
		t.Errorf("findings must survive the hung lookup (ci-failing reported without dedup): %v", keys)
	}
}

func TestGitHubPort_BudgetReturnsPartialThroughRunPort(t *testing.T) {
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{pr(6, "artie", 3*time.Hour, 3*time.Hour, "PENDING")}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, slow: 700 * time.Millisecond}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	oldMargin, oldTimeout := budgetMargin, PortTimeout
	budgetMargin, PortTimeout = 200*time.Millisecond, 1200*time.Millisecond
	defer func() { budgetMargin, PortTimeout = oldMargin, oldTimeout }()
	findings, err := runPort(context.Background(), p, in)
	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "time budget") {
		t.Fatalf("the port must return a partial result before PortTimeout, got %v", err)
	}
	if len(findings) == 0 {
		t.Error("findings from the first repo must survive the cut")
	}
}

// twoPages builds a two-page review answer: page 1 holds an old approval
// and points at page 2 ("p1"), which holds kevin's newer CHANGES_REQUESTED.
func twoPages() []*ghPRPage {
	p1, p2 := &ghPRPage{}, &ghPRPage{}
	old := ghReview{Author: ghActor{Login: "kevin"}, State: "APPROVED", SubmittedAt: ago(40 * time.Hour)}
	old.Commit.Oid = shaB
	p1.Reviews.Nodes = []ghReview{old}
	p1.Reviews.PageInfo.HasNextPage, p1.Reviews.PageInfo.EndCursor = true, "p1"
	newer := ghReview{Author: ghActor{Login: "kevin"}, State: "CHANGES_REQUESTED", SubmittedAt: ago(20 * time.Hour)}
	newer.Commit.Oid = shaA
	p2.Reviews.Nodes = []ghReview{newer}
	return []*ghPRPage{p1, p2}
}

func TestGitHubPort_PagingFollowsCursorsAndNeverReadsErrorsAsComplete(t *testing.T) {
	h := time.Hour
	newApp := func() *ghRepoData {
		app := &ghRepoData{}
		app.Open.Nodes = []ghPR{pr(60, "artie", 60*h, 40*h, "SUCCESS", truncatedReviews)}
		return app
	}
	// 1. Both pages are read: the newest review (page 2) drives the finding and the port is clean.
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": newApp(), "acme/extra": {}}, pages: map[int][]*ghPRPage{60: twoPages()}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	findings, err := p.Sweep(context.Background(), in)
	if err != nil {
		t.Fatalf("two clean pages must not be partial: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "changes-requested-stale" && f.Key == "acme/app#60" {
			found = true
		}
	}
	if !found {
		t.Error("the review on page 2 must be seen (cursor not followed)")
	}
	// 2. Page 2 answers with errors and null connections: not complete, noted, partial.
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": newApp(), "acme/extra": {}}, pages: map[int][]*ghPRPage{60: twoPages()}, pageErr: 2}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	_, err = p.Sweep(context.Background(), in)
	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "acme/app#60: reviews or threads could not be paged in full") {
		t.Fatalf("an errored page must be reported, got %v", err)
	}
	// 3. Endless pages hit the cap: noted, partial.
	loop := &ghPRPage{}
	loop.Reviews.PageInfo.HasNextPage, loop.Reviews.PageInfo.EndCursor = true, "p0"
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": newApp(), "acme/extra": {}}, pages: map[int][]*ghPRPage{60: {loop}}}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	_, err = p.Sweep(context.Background(), in)
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "could not be paged in full") {
		t.Fatalf("hitting the page cap must be reported, got %v", err)
	}
}

func TestGitHubPort_HungPatrolCostsOneRigNotTheSweep(t *testing.T) {
	h := time.Hour
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{pr(7, "someone", 30*h, 2*h, "FAILURE")}
	other := &ghRepoData{}
	other.Open.Nodes = []ghPR{pr(6, "artie", 3*h, 3*h, "PENDING"), pr(8, "someone", 30*h, 2*h, "FAILURE")}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": other}, patrolDelay: 5 * time.Second}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	p.repos = func() ([]repoRef, error) {
		return []repoRef{{Owner: "acme", Name: "app", Rig: "app", Reviewer: "phaware-val", Approver: "kevin"}, {Owner: "acme", Name: "extra", Rig: "extra", Reviewer: "phaware-val"}}, nil
	}
	oldWait, oldMargin := patrolWait, budgetMargin
	patrolWait, budgetMargin = 150*time.Millisecond, 200*time.Millisecond
	defer func() { patrolWait, budgetMargin = oldWait, oldMargin }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	findings, err := p.Sweep(ctx, in)
	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "did not answer") {
		t.Fatalf("a hung lookup must be noted as such, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the lookup must be bounded on its own timer, took %s", time.Since(start))
	}
	keys := map[string]bool{}
	for _, f := range findings {
		keys[f.Kind+"/"+f.Key] = true
	}
	if !keys["ci-failing/acme/app#7"] || !keys["no-review-loop/acme/extra#6"] || !keys["ci-failing/acme/extra#8"] {
		t.Errorf("the second repo must still be checked after a hung lookup in the first: %v", keys)
	}
	if fx.patrolCalls.Load() != 1 {
		t.Errorf("one hung bd must stop further lookups this sweep (shared Dolt), got %d calls", fx.patrolCalls.Load())
	}
	if !strings.Contains(partial.Reason, "bd unavailable this sweep") {
		t.Errorf("later rigs must carry the sweep-scoped reason: %s", partial.Reason)
	}
}

func TestGitHubPort_GraphQLErrorGuardsPinnedSeparately(t *testing.T) {
	h := time.Hour
	newApp := func() *ghRepoData {
		app := &ghRepoData{}
		app.Open.Nodes = []ghPR{pr(60, "artie", 60*h, 40*h, "SUCCESS", truncatedReviews)}
		return app
	}
	var partial *PartialError
	// prPage: an errors entry with valid-looking data is still an error.
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": newApp(), "acme/extra": {}}, pages: map[int][]*ghPRPage{60: twoPages()}, pageErr: 2, pageErrMode: "errors"}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	if _, err := p.Sweep(context.Background(), in); !errors.As(err, &partial) || !strings.Contains(partial.Reason, "could not be paged in full") {
		t.Errorf("errors with data must not read as complete: %v", err)
	}
	// prPage: null connections without an errors entry are an error too.
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": newApp(), "acme/extra": {}}, pages: map[int][]*ghPRPage{60: twoPages()}, pageErr: 2, pageErrMode: "null"}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	if _, err := p.Sweep(context.Background(), in); !errors.As(err, &partial) || !strings.Contains(partial.Reason, "could not be paged in full") {
		t.Errorf("null connections must not read as an empty page: %v", err)
	}
	// repo(): a field-level error keeps the repo's PRs; the PR whose reviews came
	// back null is paged (kevin's CHANGES_REQUESTED lives on the page) and the
	// error is noted.
	paged := &ghPRPage{}
	hidden := ghReview{Author: ghActor{Login: "kevin"}, State: "CHANGES_REQUESTED", SubmittedAt: ago(20 * h)}
	hidden.Commit.Oid = shaA
	paged.Reviews.Nodes = []ghReview{hidden}
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{pr(70, "artie", 60*h, 40*h, "SUCCESS"), pr(71, "artie", 3*h, 3*h, "PENDING")}
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, fieldErrPR: 70, paged: map[int]*ghPRPage{70: paged}}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	findings, err := p.Sweep(context.Background(), in)
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "graphql: Resource not accessible") {
		t.Fatalf("a field-level error must be noted, not fatal: %v", err)
	}
	keys := map[string]bool{}
	for _, f := range findings {
		keys[f.Kind+"/"+f.Key] = true
	}
	if !keys["changes-requested-stale/acme/app#70"] || !keys["no-review-loop/acme/app#71"] {
		t.Errorf("the repo's PRs must still be checked and the null connection paged: %v", keys)
	}
	// repo(): a denied NON_NULL field nulls the whole node; it is named and
	// skipped, never paged as phantom PR #0, and the other PR is checked.
	app = &ghRepoData{}
	app.Open.Nodes = []ghPR{pr(80, "artie", 60*h, 40*h, "SUCCESS"), pr(81, "artie", 3*h, 3*h, "PENDING")}
	fx = &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, nullNodePR: 80}
	p, in = newGitHubTest(t, fx, []string{"artie"})
	findings, err = p.Sweep(context.Background(), in)
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "open PR at index 0 came back null") {
		t.Fatalf("a null node must be named in the partial note: %v", err)
	}
	for _, n := range fx.pageQueries {
		if n == 0 {
			t.Error("a null node must never be paged as PR #0")
		}
	}
	keys = map[string]bool{}
	for _, f := range findings {
		keys[f.Kind+"/"+f.Key] = true
	}
	if !keys["no-review-loop/acme/app#81"] {
		t.Errorf("the other PR must still be checked: %v", keys)
	}
}

func authoredBy(login string) func(*ghPR) { return func(p *ghPR) { p.Author.Login = login } }

func TestGitHubPort_LinkedKindsRound1Fixes(t *testing.T) {
	h := time.Hour
	app := &ghRepoData{}
	app.Open.Nodes = []ghPR{
		// 90: not ours — a stranger's PR naming an issue must not produce a reassign suggestion.
		pr(90, "stranger", 40*h, 40*h, "SUCCESS", branch("feat/P23-90"), authoredBy("stranger"), requested("kevin", 30*h)),
		// 91: mixed-case login in the request still matches the listed human.
		pr(91, "artie", 40*h, 40*h, "SUCCESS", branch("feat/P23-91"), requested("Kevin", 30*h)),
		// 92: changes requested then approved on the same head → no hand-back.
		pr(92, "artie", 40*h, 20*h, "SUCCESS", branch("feat/P23-92"), review("kevin", "CHANGES_REQUESTED", shaA, 5*h), review("kevin", "APPROVED", shaA, 2*h)),
		// 94: one bogus key in the title must not blind the real one.
		pr(94, "artie", 40*h, 40*h, "SUCCESS", branch("feat/P23-94"), func(p *ghPR) { p.Title = "P23-99999999 and P23-94" }, requested("kevin", 30*h)),
	}
	// 93: the release tag sits on the merge commit itself (dated 1s before mergedAt).
	app.Merged.Nodes = []ghPR{mergedPR(93, "release/2.0", "feat/P23-93", 48*h, shaM)}
	app.Tags.Nodes = []ghTag{tagAt(shaM, 48*h+time.Second)}
	fx := &ghFixture{repos: map[string]*ghRepoData{"acme/app": app, "acme/extra": {}}, jiraBadKey: "P23-99999999",
		issues: []map[string]any{
			issue("P23-90", "In Progress", "indeterminate", personaID, 100*h, 1*h, "stranger's key"),
			issue("P23-91", "In Progress", "indeterminate", personaID, 100*h, 1*h, "needs handoff"),
			issue("P23-92", "In Review", "indeterminate", humanID, 100*h, 1*h, "approved after changes"),
			issue("P23-94", "In Progress", "indeterminate", personaID, 100*h, 1*h, "real key next to a bogus one"),
		}}
	p, in := newGitHubTest(t, fx, []string{"artie"})
	withJira(p, in, p.apiBase)
	in.Settings.Assistant.Sweep.Jira.Reviewers = map[string]string{"KEVIN": humanID} // mis-cased config key
	findings, err := p.Sweep(context.Background(), in)
	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Reason, "could not be looked up") {
		t.Fatalf("the bogus key must be noted, not fatal: %v", err)
	}
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f
	}
	if _, ok := got["review-handoff/P23-90"]; ok {
		t.Error("a stranger's PR must never yield a review-handoff")
	}
	if f, ok := got["review-handoff/P23-91"]; !ok || f.Fields["human"] != "kevin" || f.Fields["account_id"] != humanID {
		t.Errorf("mixed-case login and config key must still match: %+v (%v)", f, keysOf(got))
	}
	if _, ok := got["review-handoff/P23-92"]; ok {
		t.Error("an approval after changes-requested on the same head is not a hand-back")
	}
	if _, ok := got["release-unclosed/acme/app#93"]; ok {
		t.Error("a tag on the merge commit itself means the release is closed")
	}
	if _, ok := got["review-handoff/P23-94"]; !ok {
		t.Errorf("a bogus key in the same title must not blind the real key: %v", keysOf(got))
	}
}
