package git

import (
	"fmt"
	"strings"
	"testing"
)

// checksFixture is what the stubbed GitHub knows about one PR. Empty fields
// mean "none".
type checksFixture struct {
	checkRuns  string // NDJSON check runs on the head SHA
	statuses   string // NDJSON commit statuses on the head SHA
	rules      string // NDJSON active rules on the base branch
	branch     string // the base branch object
	rulesError string // stderr of a failing rules request
	movedHead  string // head SHA reported from the second lookup on
}

func stubChecks(t *testing.T, f checksFixture) *Git {
	t.Helper()
	if f.branch == "" {
		f.branch = `{"protected":false}`
	}
	rules := fmt.Sprintf("cat <<'JSON'\n%s\nJSON", f.rules)
	if f.rulesError != "" {
		rules = fmt.Sprintf("echo '%s' >&2; exit 1", f.rulesError)
	}
	later := "head1"
	if f.movedHead != "" {
		later = f.movedHead
	}
	seen := t.TempDir() + "/seen"
	// The PR-level rollup always claims a green check, standing in for GitHub
	// still reporting the previous head's results.
	return stubGh(t, fmt.Sprintf(`
case "$*" in
*"pr view"*)
	sha=head1
	[ -e %[1]q ] && sha=%[2]s
	: > %[1]q
	printf '{"headRefOid":"%%s","baseRefName":"release/1","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}' "$sha" ;;
*repos/example/repo/commits/head1/check-runs*)
cat <<'JSON'
%[3]s
JSON
;;
*repos/example/repo/commits/head1/status*)
cat <<'JSON'
%[4]s
JSON
;;
*repos/example/repo/rules/branches/release%%2F1*)
%[5]s
;;
*repos/example/repo/branches/release%%2F1*)
cat <<'JSON'
%[6]s
JSON
;;
*) echo "unexpected gh $*" >&2; exit 1 ;;
esac
`, seen, later, f.checkRuns, f.statuses, rules, f.branch))
}

func requireRule(contexts ...string) string {
	checks := make([]string, len(contexts))
	for i, c := range contexts {
		checks[i] = fmt.Sprintf(`{"context":%q}`, c)
	}
	return `{"type":"required_status_checks","parameters":{"required_status_checks":[` +
		strings.Join(checks, ",") + `]}}`
}

func checkRun(name, status, conclusion string) string {
	return fmt.Sprintf(`{"name":%q,"status":%q,"conclusion":%q,"app":{"id":7}}`, name, status, conclusion)
}

func passed(name string) string { return checkRun(name, "completed", "success") }

func TestGhPrChecksRollup(t *testing.T) {
	tests := []struct {
		name  string
		f     checksFixture
		state string
		done  bool
	}{
		{"new head with zero checks", checksFixture{}, "NO_CHECKS", false},
		{"new head with zero checks and a required check",
			checksFixture{rules: requireRule("build")}, "NO_CHECKS", false},
		{"only a non-required check finished",
			checksFixture{rules: requireRule("build"), checkRuns: passed("lint")}, "PENDING", false},
		{"required check in progress",
			checksFixture{rules: requireRule("build"), checkRuns: passed("lint") + "\n" + checkRun("build", "in_progress", "")}, "PENDING", false},
		{"one of two required checks registered",
			checksFixture{rules: requireRule("build", "test"), checkRuns: passed("build")}, "PENDING", false},
		{"all required checks passed",
			checksFixture{rules: requireRule("build", "legacy"), checkRuns: passed("build"),
				statuses: `{"context":"legacy","state":"success"}`}, "SUCCESS", true},
		{"skipped required check satisfies it",
			checksFixture{rules: requireRule("build"), checkRuns: checkRun("build", "completed", "skipped")}, "SUCCESS", true},
		{"no required checks and everything passed",
			checksFixture{checkRuns: passed("lint")}, "SUCCESS", true},
		{"non-required check still running",
			checksFixture{rules: requireRule("build"), checkRuns: passed("build") + "\n" + checkRun("lint", "queued", "")}, "PENDING", false},
		{"failed check run",
			checksFixture{checkRuns: checkRun("build", "completed", "failure")}, "FAILURE", true},
		{"failed status",
			checksFixture{statuses: `{"context":"legacy","state":"error"}`}, "FAILURE", true},
		{"pending status",
			checksFixture{statuses: `{"context":"legacy","state":"pending"}`}, "PENDING", false},
		{"required check reported by another app",
			checksFixture{
				rules:     `{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":99}]}}`,
				checkRuns: passed("build"),
			}, "PENDING", false},
		{"required by classic branch protection",
			checksFixture{
				branch:    `{"protection":{"required_status_checks":{"enforcement_level":"everyone","checks":[{"context":"build","app_id":null}]}}}`,
				checkRuns: passed("lint"),
			}, "PENDING", false},
		{"disabled classic protection requires nothing",
			checksFixture{
				branch:    `{"protection":{"required_status_checks":{"enforcement_level":"off","contexts":["build"]}}}`,
				checkRuns: passed("lint"),
			}, "SUCCESS", true},
		{"plan without rulesets",
			checksFixture{
				rulesError: "gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)",
				checkRuns:  passed("lint"),
			}, "SUCCESS", true},
		{"head moved while reading checks",
			checksFixture{checkRuns: passed("lint"), movedHead: "head2"}, "PENDING", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, done, err := stubChecks(t, tt.f).GhPrChecksRollup(1)
			if err != nil {
				t.Fatal(err)
			}
			if state != tt.state || done != tt.done {
				t.Fatalf("got (%s, %v), want (%s, %v)", state, done, tt.state, tt.done)
			}
		})
	}
}

// Unknown required checks must not read as "none required".
func TestGhPrChecksRollup_UnreadableRules(t *testing.T) {
	g := stubChecks(t, checksFixture{rulesError: "gh: Server Error (HTTP 500)", checkRuns: passed("lint")})
	if state, done, err := g.GhPrChecksRollup(1); err == nil {
		t.Fatalf("got (%s, %v), want an error", state, done)
	}
}
