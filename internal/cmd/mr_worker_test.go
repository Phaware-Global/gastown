package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The worker recorded on a merge request receives that merge request's failure
// and rejection notices, so a wrong name is worse than none.
func TestMergeRequestWorker(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		caller string // polecat name; "" is a caller with no identity
		crew   bool
		issue  string
		want   string
	}{
		{name: "unambiguous branch, unknown caller",
			branch: "polecat/keeper-gt-abc12", issue: "gt-abc12", want: "keeper"},
		{name: "owner of a hyphenated name",
			branch: "polecat/bullet-farmer-gt-abc12", caller: "bullet-farmer", issue: "gt-abc12", want: "bullet-farmer"},
		{name: "ambiguous branch, unknown caller",
			branch: "polecat/bullet-farmer-gt-abc12", issue: "gt-abc12", want: ""},
		{name: "ambiguous branch, shorter-named caller",
			branch: "polecat/bullet-farmer-gt-abc12", caller: "bullet", issue: "gt-zzz99", want: ""},
		{name: "ambiguous branch, owner on another issue",
			branch: "polecat/bullet-farmer-gt-abc12", caller: "bullet-farmer", issue: "gt-zzz99", want: ""},
		{name: "ambiguous branch, crew caller",
			branch: "polecat/bullet-farmer-gt-abc12", caller: "bullet", crew: true, issue: "gt-abc12", want: ""},
		{name: "timestamp branch, shorter-named caller",
			branch: "polecat/bullet-farmer-mk0vro62", caller: "bullet", issue: "gt-zzz99", want: "bullet-farmer"},
		{name: "timestamp branch, crew caller",
			branch: "polecat/bullet-farmer-mk0vro62", caller: "bullet", crew: true, issue: "gt-zzz99", want: "bullet-farmer"},
		{name: "bead suffix shaped like a timestamp, owner",
			branch: "polecat/keeper-gt-abcd1234", caller: "keeper", issue: "gt-abcd1234", want: "keeper"},
	}

	commands := []struct {
		name string
		run  func(t *testing.T, issue string) error
	}{
		{"mq submit", runMqSubmitForTest},
		{"done", runDoneForTest},
	}

	for _, command := range commands {
		for _, tt := range tests {
			if tt.crew && command.name == "done" {
				continue // gt done is for polecats only
			}
			t.Run(command.name+"/"+tt.name, func(t *testing.T) {
				createLog := setupMergeRequestTown(t, tt.branch, tt.caller, tt.crew)
				var err error
				output := captureStdout(t, func() { err = command.run(t, tt.issue) })
				if err != nil {
					t.Fatalf("gt %s: %v\n%s", command.name, err, output)
				}
				created, err := os.ReadFile(createLog)
				if err != nil {
					t.Fatalf("no merge request was created: %v\n%s", err, output)
				}
				got := ""
				for _, line := range strings.Split(string(created), "\n") {
					if value, ok := strings.CutPrefix(line, "worker: "); ok {
						got = value
					}
				}
				if got != tt.want {
					t.Errorf("merge request worker = %q, want %q", got, tt.want)
				}
			})
		}
	}
}

func runMqSubmitForTest(t *testing.T, issue string) error {
	t.Helper()
	oldIssue, oldNoCleanup, oldSkipDeps := mqSubmitIssue, mqSubmitNoCleanup, mqSubmitSkipDeps
	t.Cleanup(func() {
		mqSubmitIssue, mqSubmitNoCleanup, mqSubmitSkipDeps = oldIssue, oldNoCleanup, oldSkipDeps
	})
	mqSubmitIssue, mqSubmitNoCleanup, mqSubmitSkipDeps = issue, true, true
	return runMqSubmit(mqSubmitCmd, nil)
}

func runDoneForTest(t *testing.T, issue string) error {
	t.Helper()
	oldIssue, oldStatus := doneIssue, doneStatus
	t.Cleanup(func() { doneIssue, doneStatus = oldIssue, oldStatus })
	doneIssue, doneStatus = issue, ExitCompleted
	return runDone(doneCmd, nil)
}

// setupMergeRequestTown builds a throwaway town whose worktree is on a pushed
// branch, puts a fake bd on PATH, and chdirs into the worktree. It returns the
// file the fake bd writes its create arguments to.
func setupMergeRequestTown(t *testing.T, branch, caller string, crew bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script bd stub")
	}
	root := t.TempDir()
	town := filepath.Join(root, "town")
	origin := filepath.Join(root, "origin.git")
	worktree := filepath.Join(town, "testrig", "polecats", "sandbox", "testrig")
	binDir := filepath.Join(root, "bin")
	createLog := filepath.Join(root, "create.log")
	for _, dir := range []string{filepath.Join(town, "mayor"), worktree, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(path, body string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(town, "mayor", "town.json"), `{"name":"test"}`, 0o644)
	writeFile(filepath.Join(town, "mayor", "rigs.json"),
		`{"version":1,"rigs":{"testrig":{"git_url":"file:///unused","beads":{"prefix":"gt"}}}}`, 0o644)
	for _, dir := range []string{filepath.Join(town, ".beads"), filepath.Join(town, "testrig", ".beads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(town, ".beads", "routes.jsonl"), `{"prefix":"gt-","path":"testrig"}`+"\n", 0o644)
	writeFile(filepath.Join(binDir, "bd"), `#!/bin/sh
issue='"title":"work","status":"open","priority":2,"issue_type":"task"'
case " $* " in
  *" create "*) printf '%s\n' "$@" > "$BD_CREATE_LOG"; echo "{\"id\":\"gt-wisp-mr1\",$issue}" ;;
  *" show "*gt-wisp-mr1*) echo "[{\"id\":\"gt-wisp-mr1\",$issue}]" ;;
  *" show "*) echo "[{\"id\":\"gt-abc12\",$issue}]" ;;
  *" version "*) echo "bd version 1.1.0" ;;
  *) echo '[]' ;;
esac
`, 0o755)
	writeFile(filepath.Join(binDir, "tmux"), "#!/bin/sh\nexit 1\n", 0o755)
	writeFile(filepath.Join(root, "gitconfig"),
		"[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n", 0o644)

	env := map[string]string{
		"HOME":              root,
		"GIT_CONFIG_GLOBAL": filepath.Join(root, "gitconfig"),
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_CREATE_LOG":     createLog,
		"GT_TOWN_ROOT":      town,
		"GT_ROOT":           town,
		"GT_RIG":            "testrig",
		"GT_ROLE":           "",
		"GT_POLECAT":        "",
		"GT_POLECAT_PATH":   "",
		"GT_CREW":           "",
		"GT_SESSION":        "",
		"GT_BRANCH":         "",
		"BD_ACTOR":          "",
		"BEADS_DIR":         "",
		"TMUX":              "",
		"TMUX_PANE":         "",
	}
	switch {
	case crew:
		env["GT_ROLE"], env["GT_CREW"] = "testrig/crew/"+caller, caller
		env["BD_ACTOR"] = env["GT_ROLE"]
	case caller != "":
		env["GT_ROLE"], env["GT_POLECAT"] = "testrig/polecats/"+caller, caller
		env["BD_ACTOR"] = env["GT_ROLE"]
	}
	for key, value := range env {
		t.Setenv(key, value)
	}

	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "--bare", "-b", "main", origin)
	git(worktree, "init", "-b", "main")
	writeFile(filepath.Join(worktree, "a.txt"), "a\n", 0o644)
	writeFile(filepath.Join(worktree, ".gitignore"), ".beads/\n.runtime/\n", 0o644)
	git(worktree, "add", ".")
	git(worktree, "commit", "-m", "base")
	git(worktree, "remote", "add", "origin", origin)
	git(worktree, "push", "origin", "main")
	git(worktree, "checkout", "-b", branch)
	writeFile(filepath.Join(worktree, "b.txt"), "b\n", 0o644)
	git(worktree, "add", ".")
	git(worktree, "commit", "-m", "work")
	git(worktree, "push", "origin", branch)
	git(worktree, "fetch", "origin")
	t.Chdir(worktree)
	return createLog
}
