// Package prodguard keeps test binaries away from a town's shared Dolt
// server. It depends only on the standard library so that any package's tests
// can import it.
package prodguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const (
	// productionDoltPort is the port a town's shared Dolt server listens on.
	productionDoltPort = "3307"
	// deadDoltPort is a port nothing listens on, so that a client which takes
	// its port from the environment cannot fall back to the production one.
	deadDoltPort = "1"
)

// Install keeps a test binary away from the town's shared Dolt server.
// Call it first in TestMain and pass the result of m.Run through the returned
// function:
//
//	done := prodguard.Install()
//	os.Exit(done(m.Run()))
//
// It drops the variables that point at a live town or its tmux server, points
// GT_DOLT_PORT and BEADS_DOLT_PORT at a dead port, and puts wrappers for bd and
// gt first on PATH. A wrapper refuses a call that names port 3307, or that runs against the
// town or the source checkout (whose .beads redirects to the town's database),
// and the returned function then fails the binary, even if the calling test
// ignored the error. Tests that put their own stub first on PATH never reach
// the wrappers.
//
// A tool that is not installed gets no wrapper, so tests that skip without it
// still skip. Wrappers are shell scripts and are not installed on Windows.
func Install() func(code int) int {
	roots := protectedRoots()
	for _, key := range []string{
		"GT_TOWN_ROOT", "GT_ROOT", "BEADS_DIR",
		// A bare tmux client follows $TMUX onto the town's server, whose
		// panes inherit the production environment.
		"TMUX", "TMUX_PANE", "GT_TOWN_SOCKET",
	} {
		_ = os.Unsetenv(key)
	}
	// Unset, these fall back to the production port in code that connects
	// without running bd or gt.
	_ = os.Setenv("GT_DOLT_PORT", deadDoltPort)
	_ = os.Setenv("BEADS_DOLT_PORT", deadDoltPort)
	_ = os.Setenv("BEADS_DOLT_AUTO_START", "0")

	if runtime.GOOS == "windows" {
		return func(code int) int { return code }
	}
	dir, err := os.MkdirTemp("", "gt-prodguard-*")
	if err != nil {
		panic(fmt.Sprintf("production guard: %v", err))
	}
	violations := filepath.Join(dir, "violations")
	for _, tool := range []string{"bd", "gt"} {
		target, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		script := guardScript(tool, target, violations, roots)
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil { //nolint:gosec // must be executable
			panic(fmt.Sprintf("production guard: %v", err))
		}
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func(code int) int {
		defer func() { _ = os.RemoveAll(dir) }()
		refused, err := os.ReadFile(violations)
		if err != nil || len(refused) == 0 {
			return code
		}
		fmt.Fprintf(os.Stderr, "FAIL: production guard refused these calls; give the test a stub on PATH or its own Dolt server:\n%s", refused)
		return 1
	}
}

// protectedRoots lists the directories a test must not run bd or gt against:
// the town named by the environment, any town containing the working
// directory, and the source checkout.
func protectedRoots() []string {
	var roots []string
	add := func(dir string) {
		if dir == "" || filepath.Dir(dir) == dir {
			return
		}
		roots = append(roots, filepath.Clean(dir))
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			roots = append(roots, resolved)
		}
	}
	add(os.Getenv("GT_TOWN_ROOT"))
	add(os.Getenv("GT_ROOT"))

	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("production guard: %v", err))
	}
	inModule := false
	for ; filepath.Dir(dir) != dir; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !inModule {
			inModule = true
			add(dir)
		}
		if _, err := os.Stat(filepath.Join(dir, "mayor", "town.json")); err == nil {
			add(dir)
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

// guardScript renders the wrapper for one tool. The protected paths are written
// into the script because callers often rebuild the subprocess environment.
func guardScript(tool, target, violations string, roots []string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	var rootCases strings.Builder
	for _, root := range roots {
		fmt.Fprintf(&rootCases, "\t%[1]s/*|*\\ %[1]s/*|*\\ %[1]s\\ *) why=\"it runs against $d\" ;;\n", quote(root))
	}
	return `#!/bin/sh
real=` + quote(target) + `
# Version and help probes open no database.
case "$*" in
version|--version|"--allow-stale version"|help|--help|*" --help")
	cd / && unset BEADS_DIR && BEADS_DOLT_PORT=` + deadDoltPort + ` exec "$real" "$@" ;;
esac
why=
for p in "${BEADS_DOLT_PORT-}" "${GT_DOLT_PORT-}"; do
	[ "$p" = ` + productionDoltPort + ` ] && why="its environment names port ` + productionDoltPort + `"
done
case " $* " in
*"port ` + productionDoltPort + ` "*|*"port=` + productionDoltPort + ` "*) why="its arguments name port ` + productionDoltPort + `" ;;
esac
for d in "$(pwd -P)" "${BEADS_DIR-}" "$*"; do
	case "$d/" in
` + rootCases.String() + `	esac
done
if [ -n "$why" ]; then
	msg="` + tool + ` $* (cwd $(pwd -P)): $why"
	printf '%s\n' "$msg" >>` + quote(violations) + `
	echo "production guard refused: $msg" >&2
	exit 97
fi
export BEADS_DOLT_PORT="${BEADS_DOLT_PORT:-` + deadDoltPort + `}"
export GT_DOLT_PORT="${GT_DOLT_PORT:-` + deadDoltPort + `}"
exec "$real" "$@"
`
}

// Stub puts a stand-in for each tool first on PATH for the duration of t. The
// stand-in only exits 1, which a caller sees as a failed lookup. Use it in a
// test that runs code which calls bd or gt but does not depend on the answer.
func Stub(t testing.TB, tools ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		name, script := tool, "#!/bin/sh\nexit 1\n"
		if runtime.GOOS == "windows" {
			name, script = tool+".bat", "@exit /b 1\r\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // must be executable
			t.Fatalf("writing %s stub: %v", tool, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
