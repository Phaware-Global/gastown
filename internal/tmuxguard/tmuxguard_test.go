package tmuxguard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmuxguard"
)

func init() { tmuxguard.Isolate() }

func TestIsolateLeavesNoWayToTheRunningServer(t *testing.T) {
	for _, name := range []string{"TMUX", "TMUX_PANE", "GT_TOWN_SOCKET", "GT_TMUX_SOCKET"} {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("%s is still set", name)
		}
	}
	if dir := os.Getenv("TMUX_TMPDIR"); !strings.Contains(filepath.Base(dir), "gt-tmux-") {
		t.Errorf("TMUX_TMPDIR is not a private directory")
	}
}

func TestCheckRefusesTheTownSocket(t *testing.T) {
	town := t.TempDir()
	t.Setenv("GT_TOWN_ROOT", town)
	t.Setenv("TMUX", "/nowhere/tmux-1/parent,1,0")
	tmuxguard.Isolate() // re-arms from the environment above

	refused := func(socket string) (panicked bool) {
		defer func() { panicked = recover() != nil }()
		tmuxguard.Check(socket)
		return false
	}
	for _, socket := range []string{tmuxguard.TownSocketName(town), "parent"} {
		if !refused(socket) {
			t.Errorf("Check(%q) allowed the call", socket)
		}
	}
	for _, socket := range []string{"", "gt-test-private"} {
		if refused(socket) {
			t.Errorf("Check(%q) refused the call", socket)
		}
	}
	t.Setenv("TMUX", "/nowhere/tmux-1/parent,1,0")
	if !refused("") {
		t.Error("Check allowed following the inherited $TMUX")
	}
}

// Linking the tmux package isolates a test binary. One that can exec tmux
// without linking it has to call Isolate itself.
func TestEveryPackageThatExecsTmuxIsolates(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	list := exec.Command("go", "list", "-e", "-tags=integration,e2e,live,browser", "-test", "-f",
		`{{if eq .Name "main"}}{{.ImportPath}}|{{join .Deps " "}}{{end}}`, "./...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const module = "github.com/steveyegge/gastown/"
	linksTmux := regexp.MustCompile(regexp.QuoteMeta(module+"internal/tmux") + `( |$)`)
	execsTmux := regexp.MustCompile(`exec\.Command(Context)?\(([a-zA-Z]+, )?"tmux"|"(sh|bash)", "-c", "tmux `)

	linked := 0
	for _, line := range strings.Split(string(out), "\n") {
		path, deps, _ := strings.Cut(line, "|")
		pkg, isTest := strings.CutSuffix(path, ".test")
		if !isTest {
			continue
		}
		if linksTmux.MatchString(deps) {
			linked++
			continue
		}
		files, err := filepath.Glob(filepath.Join(root, strings.TrimPrefix(pkg, module), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		execs, isolates := false, false
		for _, file := range files {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			execs = execs || execsTmux.Match(src)
			isolates = isolates || (strings.HasSuffix(file, "_test.go") && strings.Contains(string(src), "tmuxguard.Isolate()"))
		}
		if execs && !isolates {
			t.Errorf("%s can exec tmux: call tmuxguard.Isolate() from an init function in a test file", pkg)
		}
	}
	if linked == 0 {
		t.Fatal("go list reported no test binary that links the tmux package")
	}
}
