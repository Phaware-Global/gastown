package daemon

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

type stuckWorkFixture struct {
	d       *Daemon
	logs    *strings.Builder
	mailLog string
}

// newStuckWorkFixture fakes bd, gt and tmux. Hook state exists only on the work
// beads: the agent bead carries no hook_bead. show is what a re-read of a work
// bead returns; list is what the status queries return.
func newStuckWorkFixture(t *testing.T, sessionAlive bool, list, show []map[string]string) *stuckWorkFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux, bd and gt")
	}
	binDir := t.TempDir()
	townRoot := t.TempDir()

	writeScript := func(name, body string) string {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0755); err != nil {
			t.Fatalf("writing fake %s: %v", name, err)
		}
		return path
	}
	writeJSON := func(name string, v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshaling %s: %v", name, err)
		}
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return path
	}
	byStatus := func(status string) string {
		matched := []map[string]string{}
		for _, b := range list {
			if b["status"] == status {
				matched = append(matched, b)
			}
		}
		return writeJSON(status+".json", matched)
	}
	agent := writeJSON("agent.json", []map[string]any{{
		"id": "gt-myr-polecat-mycat", "issue_type": "task", "labels": []string{"gt:agent"},
		"description": "agent_state: working", "updated_at": ago(2 * time.Hour),
	}})
	bdPath := writeScript("bd", `case "$*" in
  *--label=gt:agent*) cat `+agent+`;;
  *--status=hooked*) cat `+byStatus("hooked")+`;;
  *--status=in_progress*) cat `+byStatus("in_progress")+`;;
  *"show gt-myr-polecat-mycat"*) cat `+agent+`;;
  *show*) cat `+writeJSON("show.json", show)+`;;
  *) echo '[]';;
esac
`)
	mailLog := filepath.Join(binDir, "mail.log")
	gtPath := writeScript("gt", `echo "$*" >> `+mailLog+"\n")

	if sessionAlive {
		writeScript("tmux", `case "$*" in
  *show-environment*) exit 1;;
  *pane_current_command*pane_pid*) printf 'claude\t1\n';;
  *pane_current_command*) echo claude;;
  *pane_pid*) echo 1;;
  *) exit 0;;
esac
`)
	} else {
		writeScript("tmux", "echo \"can't find session\" >&2\nexit 1\n")
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logs := &strings.Builder{}
	return &stuckWorkFixture{
		d: &Daemon{
			config: &Config{TownRoot: townRoot},
			logger: log.New(logs, "", 0),
			tmux:   tmux.NewTmux(),
			bdPath: bdPath,
			gtPath: gtPath,
		},
		logs:    logs,
		mailLog: mailLog,
	}
}

func (f *stuckWorkFixture) mail(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading mail log: %v", err)
	}
	return string(data)
}

func ago(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format(time.RFC3339)
}

func workBead(status, assignee string, age time.Duration) []map[string]string {
	return []map[string]string{{
		"id": "gt-work1", "status": status, "assignee": assignee, "updated_at": ago(age),
	}}
}

func TestCheckRigGUPPViolations_FiresFromWorkBead(t *testing.T) {
	for _, status := range []string{"hooked", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			bead := workBead(status, "myr/polecats/mycat", 2*time.Hour)
			f := newStuckWorkFixture(t, true, bead, bead)

			f.d.checkRigGUPPViolations("myr")

			if got := f.mail(t); !strings.Contains(got, "GUPP_VIOLATION") || !strings.Contains(got, "gt-work1") {
				t.Errorf("expected GUPP_VIOLATION mail naming gt-work1, got %q (log: %s)", got, f.logs)
			}
		})
	}
}

func TestCheckRigGUPPViolations_Quiet(t *testing.T) {
	cases := map[string]struct {
		alive     bool
		assignee  string
		age       time.Duration
		heartbeat bool
	}{
		"recently updated bead": {alive: true, assignee: "myr/polecats/mycat", age: time.Minute},
		"fresh heartbeat":       {alive: true, assignee: "myr/polecats/mycat", age: 2 * time.Hour, heartbeat: true},
		"dead session":          {alive: false, assignee: "myr/polecats/mycat", age: 2 * time.Hour},
		"other rig":             {alive: true, assignee: "other/polecats/mycat", age: 2 * time.Hour},
		"not a polecat":         {alive: true, assignee: "myr/crew/mycat", age: 2 * time.Hour},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bead := workBead("hooked", tc.assignee, tc.age)
			f := newStuckWorkFixture(t, tc.alive, bead, bead)
			if tc.heartbeat {
				sessionName := session.PolecatSessionName(session.PrefixFor("myr"), "mycat")
				polecat.TouchSessionHeartbeat(f.d.config.TownRoot, sessionName)
			}

			f.d.checkRigGUPPViolations("myr")

			if got := f.mail(t); got != "" {
				t.Errorf("expected no mail, got %q", got)
			}
		})
	}
}

func TestCheckRigOrphanedWork_FiresFromWorkBead(t *testing.T) {
	for _, status := range []string{"hooked", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			bead := workBead(status, "myr/polecats/mycat", time.Hour)
			f := newStuckWorkFixture(t, false, bead, bead)

			f.d.checkRigOrphanedWork("myr")

			if got := f.mail(t); !strings.Contains(got, "ORPHANED_WORK") || !strings.Contains(got, "gt-work1") {
				t.Errorf("expected ORPHANED_WORK mail naming gt-work1, got %q (log: %s)", got, f.logs)
			}
		})
	}
}

func TestCheckRigOrphanedWork_Quiet(t *testing.T) {
	stale := workBead("hooked", "myr/polecats/mycat", time.Hour)
	cases := map[string]struct {
		alive      bool
		list, show []map[string]string
	}{
		"live session":   {alive: true, list: stale, show: stale},
		"just slung":     {list: workBead("hooked", "myr/polecats/mycat", time.Minute), show: stale},
		"closed since":   {list: stale, show: workBead("closed", "myr/polecats/mycat", time.Hour)},
		"reassigned":     {list: stale, show: workBead("hooked", "myr/polecats/other", time.Hour)},
		"no hooked work": {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newStuckWorkFixture(t, tc.alive, tc.list, tc.show)

			f.d.checkRigOrphanedWork("myr")

			if got := f.mail(t); got != "" {
				t.Errorf("expected no mail, got %q", got)
			}
		})
	}
}

func TestStuckWorkAlerts_NotRepeatedEveryHeartbeat(t *testing.T) {
	bead := workBead("hooked", "myr/polecats/mycat", 2*time.Hour)
	f := newStuckWorkFixture(t, false, bead, bead)

	f.d.checkRigOrphanedWork("myr")
	f.d.checkRigOrphanedWork("myr")

	if got := strings.Count(f.mail(t), "mail send"); got != 1 {
		t.Errorf("expected 1 mail across two heartbeats, got %d", got)
	}
}
