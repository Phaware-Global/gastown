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

func TestEnsureRoleWorktreeIntegrityRejectsDogKennelAlias(t *testing.T) {
	// The kennel-container exemption must not be reachable through a
	// differently-cased or symlinked alias of a real, deeper rig worktree.
	// isDogKennelContainer canonicalizes with filepath.EvalSymlinks and
	// counts segments on the resolved path, so both aliases below still
	// resolve to "not exempt" even though they present as 3 segments.
	t.Run("symlink alias (3-segment symlink to a real 4-segment worktree)", func(t *testing.T) {
		townRoot := t.TempDir()
		realWorktree := filepath.Join(townRoot, "deacon", "dogs", "alpha", "gastown")
		if err := os.MkdirAll(realWorktree, 0755); err != nil {
			t.Fatal(err)
		}
		aliasKennel := filepath.Join(townRoot, "deacon", "dogs", "alpha-gastown")
		if err := os.Symlink(realWorktree, aliasKennel); err != nil {
			t.Fatal(err)
		}

		err := ensureRoleWorktreeIntegrity(aliasKennel, townRoot, RoleDog)
		if !errors.Is(err, worktreeintegrity.ErrIntegrityViolation) {
			t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want ErrIntegrityViolation", err)
		}
	})

	t.Run("case-variant spelling still recognized as kennel (DEACON/DOGS)", func(t *testing.T) {
		townRoot := t.TempDir()
		cwd := filepath.Join(townRoot, "DEACON", "DOGS", "alpha")
		if err := os.MkdirAll(cwd, 0755); err != nil {
			t.Fatal(err)
		}

		if err := ensureRoleWorktreeIntegrity(cwd, townRoot, RoleDog); err != nil {
			t.Fatalf("ensureRoleWorktreeIntegrity() error = %v, want nil", err)
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
