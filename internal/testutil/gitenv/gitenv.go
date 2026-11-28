// Package gitenv isolates test binaries from git config in their environment.
// It imports no gastown package, so any package's tests can use it.
package gitenv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ClearInheritedConfig removes the command-scope git config the process
// inherited through GIT_CONFIG_COUNT, GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n.
//
// Some agent sessions set core.hooksPath that way. Command scope overrides repo
// config, so a fixture's own hooks would not run. Call it from TestMain, before
// m.Run.
func ClearInheritedConfig() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "GIT_CONFIG_COUNT" ||
			strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			_ = os.Unsetenv(name)
		}
	}
}

// RequirePassUnderHooksOverride runs the named test of this test binary again,
// in a child whose environment overrides core.hooksPath with an empty
// directory, and fails t unless that test passes there and leaves the directory
// empty. The named test must depend on a git hook running. The directory check
// covers a fixture that installs its hook at the effective hooks path, which an
// override would otherwise redirect into a directory the session trusts.
func RequirePassUnderHooksOverride(t *testing.T, name string) {
	t.Helper()
	hooks := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0="+hooks,
	)
	out, err := cmd.CombinedOutput()
	// A skipped or unmatched test also exits zero.
	if err != nil || !strings.Contains(string(out), "--- PASS: "+name+" ") {
		t.Errorf("%s did not pass under a hooks path override: %v\n%s", name, err, out)
	}
	if written, err := os.ReadDir(hooks); err != nil || len(written) > 0 {
		t.Errorf("%s wrote into the overriding hooks directory: %v %v", name, written, err)
	}
}
