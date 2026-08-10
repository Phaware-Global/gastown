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

func TestEnsureRoleWorktreeIntegrityRejectsWitnessRigCloneAlias(t *testing.T) {
	// Regression test for PR #185 review feedback (phaware-val): the home-dir
	// exemption must not be reachable through a differently-cased or
	// symlinked alias of the real witness/rig clone. isWitnessHomeWithoutClone
	// canonicalizes with filepath.EvalSymlinks and compares the "rig" segment
	// case-insensitively so both aliases below still resolve to "not exempt".
	t.Run("case-variant spelling (witness/RIG)", func(t *testing.T) {
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

	t.Run("symlink alias (witness/clone -> rig)", func(t *testing.T) {
		townRoot := t.TempDir()
		realClone := filepath.Join(townRoot, "gastown", "witness", "rig")
		if err := os.MkdirAll(realClone, 0755); err != nil {
			t.Fatal(err)
		}
		aliasClone := filepath.Join(townRoot, "gastown", "witness", "clone")
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
