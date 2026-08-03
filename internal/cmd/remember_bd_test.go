package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBdMemoryKey(t *testing.T) {
	// bd's memory commands take the key WITHOUT the memory. prefix and add it
	// back themselves; bd kv list returns it WITH the prefix. Sending the
	// prefixed form to `bd forget` is a silent no-op (it exits 0 reporting
	// found=false), so this conversion is load-bearing in both directions.
	tests := []struct{ kvKey, want string }{
		{"memory.feedback.pr-target-fork", "feedback.pr-target-fork"},
		{"memory.general.list", "general.list"},
		{"memory.legacy-untyped", "legacy-untyped"},
		{"memory.", ""},
		{"not-a-memory-key", "not-a-memory-key"},
	}
	for _, tt := range tests {
		if got := bdMemoryKey(tt.kvKey); got != tt.want {
			t.Errorf("bdMemoryKey(%q) = %q, want %q", tt.kvKey, got, tt.want)
		}
	}
}

func TestBdRememberArgs(t *testing.T) {
	args := bdRememberArgs("memory.feedback.k", "some value")

	if args[0] != "remember" {
		t.Errorf("subcommand = %q, want remember — `bd kv set` refuses memory.* keys", args[0])
	}

	i := slices.Index(args, "--key")
	if i < 0 || i+1 >= len(args) {
		t.Fatal("--key is not set")
	}
	if args[i+1] != "feedback.k" {
		t.Errorf("--key = %q, want the prefix stripped", args[i+1])
	}
	if slices.Contains(args, "memory.feedback.k") {
		t.Error("the memory. prefix reached bd; it adds its own")
	}

	// "--" must separate the value, and the value must come after it: a memory
	// body beginning with a dash is otherwise parsed as an unknown flag.
	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatal("no -- separator before the value")
	}
	if sep != len(args)-2 {
		t.Errorf("-- is at %d of %d args, want it immediately before the value", sep, len(args))
	}
	if args[len(args)-1] != "some value" {
		t.Errorf("value = %q, want it last", args[len(args)-1])
	}

	t.Run("a dash-leading value stays a value", func(t *testing.T) {
		a := bdRememberArgs("memory.general.k", "-x starts with a dash")
		if a[len(a)-1] != "-x starts with a dash" || a[len(a)-2] != "--" {
			t.Errorf("dash-leading value not protected by --: %q", a)
		}
	})
}

func TestBdForgetArgs(t *testing.T) {
	args := bdForgetArgs("memory.general.list")
	if args[0] != "forget" {
		t.Errorf("subcommand = %q, want forget", args[0])
	}
	if got := args[len(args)-1]; got != "general.list" {
		t.Errorf("key = %q, want the prefix stripped — the prefixed form is a silent no-op", got)
	}
	if !slices.Contains(args, "--json") {
		t.Error("--json is required to tell a real deletion from a no-op")
	}
}

func TestInterpretRememberResult(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantErr bool
	}{
		{"stored", `{"action":"remembered","key":"feedback.k","value":"v"}`, false},
		{"updated in place", `{"action":"updated","key":"feedback.k","value":"v"}`, false},
		{"no action reported", `{"key":"feedback.k"}`, true},
		// A bd without --json on this command still signals success by exit
		// status; don't fail a write it reported as fine.
		{"non-JSON output", `Remembered [feedback.k]: v`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := interpretRememberResult("memory.feedback.k", []byte(tt.out))
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestInterpretForgetResult(t *testing.T) {
	// The dangerous case: bd exits 0 having deleted nothing. Treating that as
	// success would let applyCompactPlan report memories as dropped while they
	// remain in the store.
	t.Run("nothing deleted is an error", func(t *testing.T) {
		err := interpretForgetResult("memory.general.list", []byte(`{"found":"false","key":"general.list"}`))
		if err == nil {
			t.Fatal("expected an error when bd deleted nothing")
		}
		if !strings.Contains(err.Error(), "general.list") {
			t.Errorf("error should name the key: %v", err)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		if err := interpretForgetResult("memory.general.list", []byte(`{"deleted":"true","key":"general.list"}`)); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("non-JSON output", func(t *testing.T) {
		if err := interpretForgetResult("memory.general.list", []byte(`Forgot [general.list]: v`)); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestBdMemoryRoundTrip drives the real `bd` binary against a scratch store.
//
// The bug this covers was a mismatch with bd's actual CLI, which no amount of
// argv unit-testing can catch: `bd kv set` began refusing every memory.* key,
// so all memory writes failed — `gt memories --compact` only after the operator
// had confirmed a plan. Skips when bd or a usable scratch store is unavailable.
func TestBdMemoryRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to bd")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}

	dir := t.TempDir()
	for _, args := range [][]string{{"git", "init", "-q", "."}, {"bd", "init", "--prefix", "bdrt"}} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("cannot provision a scratch store (%s): %v: %s", args[0], err, out)
		}
	}

	// bd resolves its store from the working directory.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	if _, err := os.Stat(filepath.Join(dir, ".beads")); err != nil {
		t.Skipf("scratch store missing: %v", err)
	}

	cases := []struct{ name, key, value string }{
		{"typed", "memory.feedback.round-trip", "a plain value"},
		{"legacy untyped", "memory.legacy-round-trip", "legacy body"},
		{"multi-line", "memory.general.multiline", "line one\n\nline three"},
		{"leading dash", "memory.general.dashy", "-x starts with a dash"},
		// `bd remember` recalls instead of storing when the content is a bare
		// existing key — unless --key is given, which it always is here.
		{"value that names another key", "memory.general.barename", "feedback.round-trip"},
		{"unicode", "memory.general.unicode", "café — naïve 日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := bdKvSet(tc.key, tc.value); err != nil {
				t.Fatalf("bdKvSet: %v", err)
			}
			kvs, err := bdKvListJSON()
			if err != nil {
				t.Fatalf("bdKvListJSON: %v", err)
			}
			if got := kvs[tc.key]; got != tc.value {
				t.Fatalf("round trip = %q, want %q", got, tc.value)
			}

			if err := bdKvSet(tc.key, tc.value+" UPDATED"); err != nil {
				t.Fatalf("update: %v", err)
			}
			if kvs, _ = bdKvListJSON(); !strings.HasSuffix(kvs[tc.key], "UPDATED") {
				t.Fatalf("update did not take: %q", kvs[tc.key])
			}

			if err := bdKvClear(tc.key); err != nil {
				t.Fatalf("bdKvClear: %v", err)
			}
			if kvs, _ = bdKvListJSON(); kvs[tc.key] != "" {
				t.Fatalf("key survived clear: %s", tc.key)
			}
		})
	}

	t.Run("clearing a key that is not there is an error", func(t *testing.T) {
		if err := bdKvClear("memory.general.definitely-absent"); err == nil {
			t.Error("expected an error when bd deleted nothing")
		}
	})
}
