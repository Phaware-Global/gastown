package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutputPrimeContext_ReviewerRendersTemplate guards the actual regression
// this fix addresses: the role→template-name mapping in outputPrimeContext must
// route RoleReviewer to its template. A non-empty return means the template
// path was used; an empty string means it fell through to the unknown fallback
// (the bug that made reviewers mail the refinery instead of posting).
func TestOutputPrimeContext_ReviewerRendersTemplate(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ctx := RoleContext{
		Role:     RoleReviewer,
		Rig:      "myrig",
		TownRoot: townRoot,
		WorkDir:  filepath.Join(townRoot, "myrig", "reviewer", "rig"),
	}
	out, err := outputPrimeContext(ctx)
	if err != nil {
		t.Fatalf("outputPrimeContext(reviewer) error: %v", err)
	}
	if out == "" {
		t.Fatal("reviewer fell through to the unknown fallback (empty output) — role→template mapping is missing")
	}
	if !strings.Contains(out, "reviewer post") {
		t.Errorf("reviewer context missing the `gt reviewer post` protocol; got:\n%s", out)
	}
}

func TestOutputRoleDirectives(t *testing.T) {
	t.Parallel()

	t.Run("no directives emits nothing visible", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if strings.Contains(out, "Directives") {
			t.Errorf("expected no header when no directives, got: %s", out)
		}
	})

	t.Run("town-level directive emits town header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "polecat.md"), []byte("Always be polite."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") {
			t.Errorf("expected Town Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Always be polite.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})

	t.Run("rig-level directive emits rig header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "witness.md"), []byte("Watch closely."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RoleWitness,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Rig Directives") {
			t.Errorf("expected Rig Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Watch closely.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})

	t.Run("both levels emits combined header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "polecat.md"), []byte("Town rule."), 0644); err != nil {
			t.Fatal(err)
		}

		rigDir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, "polecat.md"), []byte("Rig rule."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town & Rig Directives") {
			t.Errorf("expected combined header, got: %s", out)
		}
		if !strings.Contains(out, "Town rule.") {
			t.Errorf("expected town content, got: %s", out)
		}
		if !strings.Contains(out, "Rig rule.") {
			t.Errorf("expected rig content, got: %s", out)
		}
	})

	t.Run("explain mode shows file paths", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, true)
		out := buf.String()

		if !strings.Contains(out, "[EXPLAIN]") {
			t.Errorf("expected EXPLAIN output, got: %s", out)
		}
		if !strings.Contains(out, filepath.Join("directives", "polecat.md")) {
			t.Errorf("expected file path in explain output, got: %s", out)
		}
	})

	t.Run("empty rig name skips rig path", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "mayor.md"), []byte("Mayor directive."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RoleMayor,
			TownRoot: townRoot,
			Rig:      "",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") {
			t.Errorf("expected Town Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Mayor directive.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})
}
