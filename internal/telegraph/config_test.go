package telegraph

import (
	"os"
	"path/filepath"
	"testing"
)

const validTOML = `
[telegraph]
listen_addr  = ":9000"
buffer_size  = 128
nudge_window = "1m"
body_cap     = 2048

[telegraph.providers.jira]
enabled    = true
secret_env = "GT_TEST_JIRA_SECRET"
events     = ["issue_created", "issue_updated"]
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "telegraph.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadConfig_Valid(t *testing.T) {
	path := writeTempConfig(t, validTOML)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Telegraph.ListenAddr != ":9000" {
		t.Errorf("listen_addr: got %q, want %q", cfg.Telegraph.ListenAddr, ":9000")
	}
	if cfg.Telegraph.BufferSize != 128 {
		t.Errorf("buffer_size: got %d, want 128", cfg.Telegraph.BufferSize)
	}
	if cfg.Telegraph.NudgeWindow != "1m" {
		t.Errorf("nudge_window: got %q, want %q", cfg.Telegraph.NudgeWindow, "1m")
	}
	if cfg.Telegraph.BodyCap != 2048 {
		t.Errorf("body_cap: got %d, want 2048", cfg.Telegraph.BodyCap)
	}
	jira := cfg.Telegraph.Providers["jira"]
	if jira == nil {
		t.Fatal("jira provider missing")
	}
	if !jira.Enabled {
		t.Error("jira.enabled: want true")
	}
	if jira.SecretEnv != "GT_TEST_JIRA_SECRET" {
		t.Errorf("jira.secret_env: got %q", jira.SecretEnv)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	const minimal = `
[telegraph]
listen_addr = ":8765"

[telegraph.providers.jira]
enabled    = false
secret_env = "GT_TELEGRAPH_JIRA_SECRET"
`
	path := writeTempConfig(t, minimal)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Telegraph.BufferSize != 256 {
		t.Errorf("default buffer_size: got %d, want 256", cfg.Telegraph.BufferSize)
	}
	if cfg.Telegraph.NudgeWindow != "30s" {
		t.Errorf("default nudge_window: got %q, want %q", cfg.Telegraph.NudgeWindow, "30s")
	}
	if cfg.Telegraph.BodyCap != 4096 {
		t.Errorf("default body_cap: got %d, want 4096", cfg.Telegraph.BodyCap)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/telegraph.toml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadConfig_InvalidTOML(t *testing.T) {
	path := writeTempConfig(t, "not valid toml ][[[")
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestLoadConfig_EnabledProviderMissingSecretEnv(t *testing.T) {
	const bad = `
[telegraph]
listen_addr = ":8765"

[telegraph.providers.jira]
enabled    = true
secret_env = ""
`
	path := writeTempConfig(t, bad)
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error: enabled provider missing secret_env")
	}
}

func TestLoadConfig_InvalidNudgeWindow(t *testing.T) {
	const bad = `
[telegraph]
listen_addr  = ":8765"
nudge_window = "not-a-duration"

[telegraph.providers.jira]
enabled    = false
secret_env = "GT_TELEGRAPH_JIRA_SECRET"
`
	path := writeTempConfig(t, bad)
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for invalid nudge_window")
	}
}

func TestResolveSecrets_HappyPath(t *testing.T) {
	t.Setenv("GT_TEST_JIRA_SECRET", "supersecret")
	path := writeTempConfig(t, validTOML)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	secrets, err := ResolveSecrets(cfg)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if secrets.Providers["jira"] != "supersecret" {
		t.Error("expected jira secret to be resolved")
	}
}

func TestResolveSecrets_UnsetEnvVar(t *testing.T) {
	os.Unsetenv("GT_TEST_JIRA_SECRET")
	path := writeTempConfig(t, validTOML)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	_, err = ResolveSecrets(cfg)
	if err == nil {
		t.Error("expected error: env var unset")
	}
}

func TestResolveSecrets_DisabledProviderSkipped(t *testing.T) {
	const disabledTOML = `
[telegraph]
listen_addr = ":8765"

[telegraph.providers.jira]
enabled    = false
secret_env = "GT_TEST_JIRA_SECRET"
`
	// env var intentionally unset — should not be required for disabled providers
	os.Unsetenv("GT_TEST_JIRA_SECRET")
	path := writeTempConfig(t, disabledTOML)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	secrets, err := ResolveSecrets(cfg)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if _, ok := secrets.Providers["jira"]; ok {
		t.Error("disabled provider should not appear in resolved secrets")
	}
}

func TestNudgeWindowDuration(t *testing.T) {
	path := writeTempConfig(t, validTOML)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	d, err := NudgeWindowDuration(cfg)
	if err != nil {
		t.Fatalf("NudgeWindowDuration: %v", err)
	}
	if d.String() != "1m0s" {
		t.Errorf("NudgeWindowDuration: got %v", d)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	got := DefaultConfigPath("/home/agent/gt")
	want := "/home/agent/gt/settings/telegraph.toml"
	if got != want {
		t.Errorf("DefaultConfigPath: got %q, want %q", got, want)
	}
}
