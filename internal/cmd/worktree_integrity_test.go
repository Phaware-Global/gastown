package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	worktreeintegrity "github.com/steveyegge/gastown/internal/worktree"
)

func TestEnsureRoleWorktreeIntegrityRequiresPolecatMetadata(t *testing.T) {
	townRoot := t.TempDir()
	cwd := filepath.Join(townRoot, "gastown", "polecats", "deathclaw", "gastown")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	err := ensureRoleWorktreeIntegrity(cwd, townRoot, RolePolecat)
	if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
	}
	if !strings.Contains(err.Error(), "gt doctor --fix") {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want remediation", err)
	}
}

func TestEnsureRoleWorktreeIntegrityAllowsWitnessWithoutMetadata(t *testing.T) {
	// Witness's home dir has no rig/ git clone by design (see `gt rig
	// --help`), so missing .git metadata directly under witness/ must not be
	// treated as a violation. Regression test for gt-8815.
	townRoot := t.TempDir()
	cwd := filepath.Join(townRoot, "gastown", "witness")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	if err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleWitness); err != nil {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want nil", err)
	}
}

func TestEnsureRoleWorktreeIntegrityRejectsWitnessRigCloneWithoutMetadata(t *testing.T) {
	// witness/rig/ is a real linked worktree when present (see
	// internal/witness/manager.go:witnessDir) and is not covered by the
	// witness-home exemption above — missing .git metadata there must still
	// fail closed. Regression test for PR #185 review feedback on gt-8815.
	townRoot := t.TempDir()
	cwd := filepath.Join(townRoot, "gastown", "witness", "rig")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleWitness)
	if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
	}
}

func TestEnsureRoleWorktreeIntegrityWitnessHomeAliasPathSpace(t *testing.T) {
	// isWitnessHomeWithoutClone is a POSITIVE match on exactly <rig>/witness
	// (two path segments) rather than a negative carve-out of "everything
	// that isn't the clone". Polarity is what makes this safe: any
	// unrecognized spelling of the witness home dir — case variance, a
	// symlinked alias, anything unforeseen — falls through to "not exempt"
	// and fails closed, since it can only ever miss segments it doesn't
	// recognize, never manufacture a match. A negative test does the
	// opposite: something that fails to look like "the clone" gets exempted
	// by that very failure, which is how an earlier version of this fix
	// would have exempted the real witness/rig clone under a case-variant or
	// symlinked spelling of "rig". Regression test for gt-s6d4.
	t.Run("case-variant spelling (witness/RIG) is three segments, not exempt", func(t *testing.T) {
		townRoot := t.TempDir()
		cwd := filepath.Join(townRoot, "gastown", "witness", "RIG")
		if err := os.MkdirAll(cwd, 0755); err != nil {
			t.Fatal(err)
		}

		err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleWitness)
		if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
			t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
		}
	})

	t.Run("symlinked alias of the real clone (witness/alias -> rig) is three segments, not exempt", func(t *testing.T) {
		townRoot := t.TempDir()
		realClone := filepath.Join(townRoot, "gastown", "witness", "rig")
		if err := os.MkdirAll(realClone, 0755); err != nil {
			t.Fatal(err)
		}
		aliasClone := filepath.Join(townRoot, "gastown", "witness", "alias")
		if err := os.Symlink(realClone, aliasClone); err != nil {
			t.Fatal(err)
		}

		err := ensureRoleWorktreeIntegrity(aliasClone, townRoot, RoleWitness)
		if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
			t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
		}
	})
}

func TestEnsureRoleWorktreeIntegrityRejectsWitnessRigCloneInheritingTownRootGit(t *testing.T) {
	// Regression test for PR #185 review feedback (phaware-val): townRoot
	// itself has real, well-formed .git metadata (the documented `gt install
	// --git` deployment). findGitMarker must not walk past the witness/rig
	// worktree boundary and hand back TownRoot's own marker — that would let
	// a witness/rig clone with its .git deleted (interrupted clone, partial
	// rsync) pass validation by inheriting the town's unrelated repo.
	townRoot := t.TempDir()
	townGitDir := filepath.Join(townRoot, ".git")
	if err := os.MkdirAll(townGitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townGitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(townRoot, "gastown", "witness", "rig")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleWitness)
	if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
	}
}

func TestEnsureRoleWorktreeIntegrityAllowsDogKennelWithoutMetadata(t *testing.T) {
	// A dog's kennel container (deacon/dogs/<name>) is a plain directory by
	// design — it never has its own .git. Regression test for gt-3zow: PR
	// #185's TownRoot-exclusive walk bound bricked every dog and boot
	// session because it made this unreachable .git a hard requirement.
	townRoot := t.TempDir()
	cwd := filepath.Join(townRoot, "deacon", "dogs", "alpha")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	if err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleDog); err != nil {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want nil", err)
	}

	bootCwd := filepath.Join(townRoot, "deacon", "dogs", "boot")
	if err := os.MkdirAll(bootCwd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ensureRoleWorktreeIntegrity(bootCwd, townRoot, RoleBoot); err != nil {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want nil", err)
	}
}

func TestEnsureRoleWorktreeIntegrityRejectsDogRigWorktreeWithoutMetadata(t *testing.T) {
	// deacon/dogs/<name>/<rig> is a real linked worktree created inside the
	// kennel (one per rig) and is not covered by the kennel-container
	// exemption above — missing .git metadata there must still fail closed.
	townRoot := t.TempDir()
	cwd := filepath.Join(townRoot, "deacon", "dogs", "alpha", "gastown")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleDog)
	if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
	}
}

func TestEnsureRoleWorktreeIntegrityDogKennelAliasPathSpace(t *testing.T) {
	// isDogKennelContainer intentionally matches findGitMarker's path space:
	// both resolve with filepath.Abs (not filepath.EvalSymlinks) and compare
	// segments exact-case (not case-folded). A canonicalizing exemption check
	// paired with a non-canonicalizing walk is exactly the bug this regression
	// test guards against — see gt-s6d4: the two disagreed about which path
	// they were describing, so on a symlinked deacon/ the exemption failed to
	// match where findGitMarker actually walked and dogs bricked.
	t.Run("symlink alias resolves as a kennel, matching findGitMarker's own unresolved walk", func(t *testing.T) {
		townRoot := t.TempDir()
		realWorktree := filepath.Join(townRoot, "deacon", "dogs", "alpha", "gastown")
		if err := os.MkdirAll(realWorktree, 0755); err != nil {
			t.Fatal(err)
		}
		aliasKennel := filepath.Join(townRoot, "deacon", "dogs", "alpha-gastown")
		if err := os.Symlink(realWorktree, aliasKennel); err != nil {
			t.Fatal(err)
		}

		// Not a brick: findGitMarker itself never resolves aliasKennel, so it
		// would walk the same literal 3-segment path and find nothing beneath
		// it either — both functions agree the container is exempt.
		if err := ensureRoleWorktreeIntegrity(aliasKennel, townRoot, RoleDog); err != nil {
			t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want nil", err)
		}
	})

	t.Run("case-variant spelling (DEACON/DOGS) is not recognized as a kennel", func(t *testing.T) {
		townRoot := t.TempDir()
		cwd := filepath.Join(townRoot, "DEACON", "DOGS", "alpha")
		if err := os.MkdirAll(cwd, 0755); err != nil {
			t.Fatal(err)
		}

		err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleDog)
		if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
			t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
		}
	})
}

func TestEnsureRoleWorktreeIntegrityAllowsNeutralDirectoryWithoutMetadata(t *testing.T) {
	townRoot := t.TempDir()

	if err := ensureRoleWorktreeIntegrity(townRoot, townRoot, RoleUnknown); err != nil {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want nil", err)
	}
}

func TestEnsureRoleWorktreeIntegrityRejectsMalformedOptionalMetadata(t *testing.T) {
	townRoot := t.TempDir()
	cwd := filepath.Join(townRoot, "scratch")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".git"), []byte("corrupted\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleUnknown)
	if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
		t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
	}
}

func TestRunMoleculeStatusExplicitTargetValidatesCallerWorktree(t *testing.T) {
	t.Setenv(EnvGTRole, "")
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(townRoot, "gastown", "polecats", "deathclaw")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	err = runMoleculeStatus(nil, []string{"gastown/polecats/toast"})
	if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
		t.Fatalf("runMoleculeStatus() error = %v, want ErrIntegrityViolation", err)
	}
}
