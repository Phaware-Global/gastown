package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// canaryGT puts a fake `gt` first on PATH that records every invocation. It
// stands in for the real binary so a leak shows up as a log entry rather than
// as mail in a live town.
func canaryGT(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "gt-invocations.log")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte(script), 0755); err != nil {
		t.Fatalf("write canary gt: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// Completing a convoy under test must never exec the `gt` found on PATH: on a
// town machine that is the live binary, and `gt mail send mayor/` from a test
// delivers a real "Convoy complete: Patrol" to the live Mayor (gt-yjput).
func TestConvoyCompletionNeverExecsGTOnPath(t *testing.T) {
	townRoot, closeLog := mockBdForRoleWorkflowTest(t, "closed")
	gtLog := canaryGT(t)

	if err := checkSingleConvoy(townRoot, "hq-wf-role", false); err != nil {
		t.Fatalf("checkSingleConvoy: %v", err)
	}
	if _, err := os.Stat(closeLog); err != nil {
		t.Fatalf("fixture did not reach the convoy-close path: %v", err)
	}
	if data, err := os.ReadFile(gtLog); err == nil {
		t.Fatalf("convoy completion exec'd gt from PATH:\n%s", data)
	}
}

// The completion notice still goes out through the seam: the Mayor gets the
// "Convoy complete" mail, via the injected sender rather than a real gt.
func TestConvoyCompletionMailsMayorThroughSeam(t *testing.T) {
	townRoot, _ := mockBdForRoleWorkflowTest(t, "closed")
	sent := recordConvoyGT(t)

	if err := checkSingleConvoy(townRoot, "hq-wf-role", false); err != nil {
		t.Fatalf("checkSingleConvoy: %v", err)
	}
	for _, call := range *sent {
		if strings.HasPrefix(call, "mail send mayor/ -s Convoy complete: Patrol -m ") {
			return
		}
	}
	t.Fatalf("no mayor completion mail among gt calls: %q", *sent)
}

// With nothing injected, the production default refuses inside a test binary.
// The canary goes on PATH first so that if the guard regresses this test fails
// against a recording stub instead of mailing the live Mayor.
func TestConvoyGTDefaultRefusesUnderTest(t *testing.T) {
	gtLog := canaryGT(t)

	err := convoyGT("mail", "send", "mayor/", "-s", "x", "-m", "y")
	if data, readErr := os.ReadFile(gtLog); readErr == nil {
		t.Fatalf("convoyGT default exec'd gt from PATH:\n%s", data)
	}
	if !errors.Is(err, errConvoyGTRefusedUnderTest) {
		t.Fatalf("convoyGT default = %v, want errConvoyGTRefusedUnderTest", err)
	}
}
