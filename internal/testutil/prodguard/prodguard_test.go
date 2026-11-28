package prodguard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	done := Install()
	os.Exit(done(m.Run()))
}

func TestGuardScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrappers are shell scripts")
	}
	tmp := t.TempDir()
	town := filepath.Join(tmp, "it's a town")
	elsewhere := filepath.Join(tmp, "elsewhere")
	for _, dir := range []string{filepath.Join(town, "rig"), elsewhere} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The wrapper compares physical paths, and macOS temp dirs are symlinked.
	resolved, err := filepath.EvalSymlinks(town)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "real-bd")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho \"port=$BEADS_DOLT_PORT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		dir     string
		env     []string
		args    []string
		refused bool
		output  string
	}{
		{name: "production port in env", dir: elsewhere, env: []string{"BEADS_DOLT_PORT=3307"}, args: []string{"show", "x"}, refused: true},
		{name: "production port in GT env", dir: elsewhere, env: []string{"GT_DOLT_PORT=3307"}, args: []string{"show", "x"}, refused: true},
		{name: "production port in args", dir: elsewhere, args: []string{"init", "--server-port", "3307"}, refused: true},
		{name: "working directory in town", dir: filepath.Join(town, "rig"), args: []string{"show", "x"}, refused: true},
		{name: "BEADS_DIR in town", dir: elsewhere, env: []string{"BEADS_DIR=" + filepath.Join(resolved, ".beads")}, args: []string{"list"}, refused: true},
		{name: "town in args", dir: elsewhere, args: []string{"-C", resolved, "list"}, refused: true},
		{name: "isolated server", dir: elsewhere, env: []string{"BEADS_DOLT_PORT=43211"}, args: []string{"show", "x"}, output: "port=43211"},
		{name: "no port gets a dead one", dir: elsewhere, args: []string{"show", "x"}, output: "port=" + deadDoltPort},
		{name: "version probe in town", dir: town, env: []string{"BEADS_DOLT_PORT=3307"}, args: []string{"version"}, output: "port=" + deadDoltPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := filepath.Join(t.TempDir(), "violations")
			wrapper := filepath.Join(t.TempDir(), "bd")
			script := guardScript("bd", target, violations, []string{town, resolved})
			if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(wrapper, tt.args...)
			cmd.Dir = tt.dir
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tt.env...)
			out, err := cmd.CombinedOutput()
			recorded, _ := os.ReadFile(violations)

			if !tt.refused {
				if err != nil || len(recorded) != 0 || !strings.Contains(string(out), tt.output) {
					t.Fatalf("err=%v recorded=%q output=%q, want pass-through with %q", err, recorded, out, tt.output)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 97 || len(recorded) == 0 {
				t.Fatalf("err=%v recorded=%q output=%q, want a recorded refusal", err, recorded, out)
			}
		})
	}
}

// A test binary that can reach bd through the beads package, or that names bd
// or gt in an exec call, has to install the guard.
func TestEveryPackageThatCanReachBDInstallsGuard(t *testing.T) {
	list := exec.Command("go", "list", "-test", "-f",
		`{{if eq .Name "main"}}{{.ImportPath}}|{{join .Deps " "}}{{end}}`, "./...")
	list.Dir = filepath.Join("..", "..", "..")
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	root, err := filepath.Abs(list.Dir)
	if err != nil {
		t.Fatal(err)
	}
	const module = "github.com/steveyegge/gastown/"
	usesBeads := regexp.MustCompile(regexp.QuoteMeta(module+"internal/beads") + `( |$)`)
	execsTool := regexp.MustCompile(`exec\.Command(Context)?\(([a-zA-Z]+, )?"(bd|gt)"`)

	checked := 0
	for _, line := range strings.Split(string(out), "\n") {
		path, deps, _ := strings.Cut(line, "|")
		pkg, isTest := strings.CutSuffix(path, ".test")
		if !isTest {
			continue
		}
		files, err := filepath.Glob(filepath.Join(root, strings.TrimPrefix(pkg, module), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		reaches, installs := usesBeads.MatchString(deps), false
		for _, file := range files {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			reaches = reaches || execsTool.Match(src)
			installs = installs || (strings.HasSuffix(file, "_test.go") && strings.Contains(string(src), "Install()") && strings.Contains(string(src), "func TestMain("))
		}
		if reaches {
			checked++
			if !installs {
				t.Errorf("%s can run bd or gt: call prodguard.Install() in its TestMain", pkg)
			}
		}
	}
	if checked == 0 {
		t.Fatal("go list reported no package that can reach bd")
	}
}
