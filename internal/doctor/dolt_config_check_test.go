package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoltConfigCheck_DetectsMissingSharedKeys(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("prefix:\nissue-prefix:\ndolt.idle-timeout: \"0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDoltConfigCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "storage.backend unset") || !strings.Contains(result.Details[0], "dolt.server unset") || !strings.Contains(result.Details[0], "dolt.port unset") {
		t.Fatalf("Details = %#v, want missing Dolt keys", result.Details)
	}
}

func TestDoltConfigCheck_DetectsPolecatRedirectConfig(t *testing.T) {
	townRoot := t.TempDir()
	targetBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	polecatBeads := filepath.Join(townRoot, "gastown", "polecats", "guzzle", "gastown", ".beads")
	if err := os.MkdirAll(targetBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(polecatBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetBeads, "metadata.json"), []byte(`{"dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetBeads, "config.yaml"), []byte("storage.backend: dolt\ndolt.server: \"127.0.0.1\"\ndolt.port: 3307\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(polecatBeads, "redirect"), []byte("../../../mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(polecatBeads, "config.yaml"), []byte("prefix:\nissue-prefix:\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDoltConfigCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	if len(check.targets) != 1 || check.targets[0].beadsDir != polecatBeads {
		t.Fatalf("targets = %#v, want only polecat beads dir", check.targets)
	}
}

func TestDoltConfigCheck_FixAddsMissingKeysOnly(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"hq"}`), 0644); err != nil {
		t.Fatal(err)
	}
	initial := "prefix: hq\nissue-prefix: hq\nexport.auto: \"false\"\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDoltConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"prefix: hq\n", "issue-prefix: hq\n", "export.auto: \"false\"\n", "storage.backend: dolt\n", "dolt.server: \"127.0.0.1\"\n", "dolt.port: 3307\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("config.yaml missing %q after fix:\n%s", want, got)
		}
	}

	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("Status after fix = %v, want %v: %s details=%v", result.Status, StatusOK, result.Message, result.Details)
	}
}

// staleMetadataTown builds a town whose Dolt server is configured on 3307 but
// whose rig metadata.json still names 3308 (what a rogue bd auto-start leaves
// behind), with a polecat worktree redirecting to that rig.
func staleMetadataTown(t *testing.T) (townRoot, polecatBeads string) {
	t.Helper()
	townRoot = t.TempDir()
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	polecatBeads = filepath.Join(townRoot, "gastown", "polecats", "guzzle", "gastown", ".beads")
	for _, dir := range []string{rigBeads, polecatBeads, filepath.Join(townRoot, ".dolt-data"), filepath.Join(townRoot, "mayor")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(townRoot, ".dolt-data", "config.yaml"): "listener:\n  port: 3307\n",
		filepath.Join(townRoot, "mayor", "rigs.json"):        `{"rigs":{"gastown":{}}}`,
		filepath.Join(rigBeads, "metadata.json"):             `{"dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3308,"dolt_database":"gastown"}`,
		filepath.Join(polecatBeads, "redirect"):              "../../../mayor/rig/.beads\n",
		filepath.Join(polecatBeads, "config.yaml"):           "prefix: gt\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return townRoot, polecatBeads
}

// A metadata.json port that disagrees with the town's configured Dolt port is
// stale. Copying it into config.yaml pins every worktree to a port nothing
// listens on (the 3308 pollution of 23 tracked configs, gt-15qi).
func TestDoltConfigCheck_FixNeverWritesStaleMetadataPort(t *testing.T) {
	townRoot, polecatBeads := staleMetadataTown(t)

	check := NewDoltConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(polecatBeads, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "3308") {
		t.Fatalf("config.yaml pinned to the stale metadata port:\n%s", got)
	}
	if !strings.Contains(got, "dolt.port: 3307\n") {
		t.Fatalf("config.yaml should carry the town's configured port 3307:\n%s", got)
	}
}

// gt doctor --fix runs DoltConfigCheck before StaleDoltPortCheck repairs the
// metadata, so the dolt-config fix must already agree with the repaired state.
func TestDoctorFix_DoltConfigAgreesWithStaleDoltPortRepair(t *testing.T) {
	townRoot, polecatBeads := staleMetadataTown(t)

	d := NewDoctor()
	d.Register(NewDoltConfigCheck())
	d.Register(NewStaleDoltPortCheck())
	d.Fix(&CheckContext{TownRoot: townRoot})

	data, err := os.ReadFile(filepath.Join(polecatBeads, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "3308") || !strings.Contains(got, "dolt.port: 3307\n") {
		t.Fatalf("polecat config.yaml disagrees with the repaired metadata port:\n%s", got)
	}
}
