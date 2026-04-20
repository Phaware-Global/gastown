package telegraph_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

func writeTOML(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writeTOML: %v", err)
	}
	return path
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	if cfg.Telegraph.ListenAddr != ":8765" {
		t.Errorf("ListenAddr = %q, want :8765", cfg.Telegraph.ListenAddr)
	}
	if cfg.Telegraph.BufferSize != 256 {
		t.Errorf("BufferSize = %d, want 256", cfg.Telegraph.BufferSize)
	}
	if cfg.Telegraph.NudgeWindow != "30s" {
		t.Errorf("NudgeWindow = %q, want 30s", cfg.Telegraph.NudgeWindow)
	}
	if cfg.Telegraph.BodyCap != 4096 {
		t.Errorf("BodyCap = %d, want 4096", cfg.Telegraph.BodyCap)
	}
	if cfg.Telegraph.Providers == nil {
		t.Error("Providers map is nil")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Parallel()
	got := telegraph.DefaultPath("/home/user/gt")
	want := "/home/user/gt/settings/telegraph.toml"
	if got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestLoad_Valid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTOML(t, dir, "telegraph.toml", `
[telegraph]
listen_addr  = ":9000"
buffer_size  = 512
nudge_window = "1m"
body_cap     = 8192

[telegraph.providers.jira]
enabled    = true
secret_env = "MY_JIRA_SECRET"
events     = ["issue_created", "issue_updated"]
`)

	cfg, err := telegraph.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Telegraph.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q", cfg.Telegraph.ListenAddr)
	}
	if cfg.Telegraph.BufferSize != 512 {
		t.Errorf("BufferSize = %d", cfg.Telegraph.BufferSize)
	}
	p := cfg.Telegraph.Providers["jira"]
	if p == nil {
		t.Fatal("jira provider not loaded")
	}
	if !p.Enabled {
		t.Error("jira.Enabled = false, want true")
	}
	if p.SecretEnv != "MY_JIRA_SECRET" {
		t.Errorf("jira.SecretEnv = %q", p.SecretEnv)
	}
	if len(p.Events) != 2 {
		t.Errorf("jira.Events len = %d, want 2", len(p.Events))
	}
}

func TestLoad_NotFound(t *testing.T) {
	t.Parallel()
	_, err := telegraph.Load("/nonexistent/telegraph.toml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoad_Malformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTOML(t, dir, "telegraph.toml", `[telegraph]
listen_addr = [not valid toml`)
	_, err := telegraph.Load(path)
	if err == nil {
		t.Error("expected error for malformed TOML")
	}
}

func TestLoadWithDefaults_MergesDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Only override listen_addr; everything else should be default.
	path := writeTOML(t, dir, "telegraph.toml", `
[telegraph]
listen_addr = ":7777"
`)
	cfg, err := telegraph.LoadWithDefaults(path)
	if err != nil {
		t.Fatalf("LoadWithDefaults: %v", err)
	}
	if cfg.Telegraph.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q, want :7777", cfg.Telegraph.ListenAddr)
	}
	if cfg.Telegraph.BufferSize != 256 {
		t.Errorf("BufferSize = %d, want 256 (default)", cfg.Telegraph.BufferSize)
	}
	if cfg.Telegraph.NudgeWindow != "30s" {
		t.Errorf("NudgeWindow = %q, want 30s (default)", cfg.Telegraph.NudgeWindow)
	}
}

func TestValidate_Valid(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "GT_JIRA_SECRET",
		Events:    []string{"issue_created"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestValidate_MissingListenAddr(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.ListenAddr = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing listen_addr")
	}
}

func TestValidate_BadNudgeWindow(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.NudgeWindow = "not-a-duration"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid nudge_window")
	}
}

func TestValidate_EnabledProviderMissingSecret(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "", // missing
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for enabled provider with no secret_env")
	}
}

func TestValidate_DisabledProviderNoSecretOK(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   false,
		SecretEnv: "", // OK for disabled provider
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for disabled provider", err)
	}
}

func TestParsedNudgeWindow(t *testing.T) {
	t.Parallel()
	tc := telegraph.TelegraphConfig{NudgeWindow: "45s"}
	d, err := tc.ParsedNudgeWindow()
	if err != nil {
		t.Fatalf("ParsedNudgeWindow: %v", err)
	}
	if d != 45*time.Second {
		t.Errorf("ParsedNudgeWindow = %v, want 45s", d)
	}
}

func TestResolveSecret_OK(t *testing.T) {
	t.Setenv("GT_TEST_JIRA_SECRET", "s3cr3t")
	p := &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "GT_TEST_JIRA_SECRET",
	}
	got, err := p.ResolveSecret()
	if err != nil {
		t.Fatalf("ResolveSecret: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("ResolveSecret = %q, want s3cr3t", got)
	}
}

func TestResolveSecret_Missing(t *testing.T) {
	t.Parallel()
	os.Unsetenv("GT_TELEGRAPH_JIRA_SECRET_MISSING")
	p := &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "GT_TELEGRAPH_JIRA_SECRET_MISSING",
	}
	_, err := p.ResolveSecret()
	if err == nil {
		t.Error("expected error for missing env var")
	}
}

func TestResolveSecret_NoEnvVarConfigured(t *testing.T) {
	t.Parallel()
	p := &telegraph.ProviderConfig{SecretEnv: ""}
	_, err := p.ResolveSecret()
	if err == nil {
		t.Error("expected error when secret_env is empty")
	}
}

func TestResolveProviders_OK(t *testing.T) {
	t.Setenv("GT_TEST_PROVIDER_SECRET", "tok3n")
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "GT_TEST_PROVIDER_SECRET",
		Events:    []string{"issue_created"},
	}
	cfg.Telegraph.Providers["github"] = &telegraph.ProviderConfig{
		Enabled:   false,
		SecretEnv: "GT_GITHUB_SECRET_UNSET",
	}

	resolved, err := cfg.ResolveProviders()
	if err != nil {
		t.Fatalf("ResolveProviders: %v", err)
	}
	if len(resolved) != 1 {
		t.Errorf("len(resolved) = %d, want 1 (disabled github skipped)", len(resolved))
	}
	r, ok := resolved["jira"]
	if !ok {
		t.Fatal("jira not in resolved map")
	}
	if r.Secret != "tok3n" {
		t.Errorf("jira Secret = %q, want tok3n", r.Secret)
	}
}

func TestResolveProviders_MissingSecret(t *testing.T) {
	t.Parallel()
	os.Unsetenv("GT_TELEGRAPH_NO_SUCH_VAR")
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "GT_TELEGRAPH_NO_SUCH_VAR",
	}
	_, err := cfg.ResolveProviders()
	if err == nil {
		t.Error("expected error when secret env var is missing")
	}
}

func TestResolvedProvider_StringRedactsSecret(t *testing.T) {
	t.Parallel()
	r := &telegraph.ResolvedProvider{
		Config: &telegraph.ProviderConfig{SecretEnv: "GT_SECRET_ENV"},
		Secret: "supersecret",
	}
	s := r.String()
	if strings.Contains(s, "supersecret") {
		t.Errorf("String() leaked secret: %s", s)
	}
	if !strings.Contains(s, "REDACTED") {
		t.Errorf("String() missing REDACTED marker: %s", s)
	}
}

func TestResolvedProvider_GoStringRedactsSecret(t *testing.T) {
	t.Parallel()
	r := &telegraph.ResolvedProvider{
		Config: &telegraph.ProviderConfig{SecretEnv: "GT_SECRET_ENV"},
		Secret: "supersecret",
	}
	s := r.GoString()
	if strings.Contains(s, "supersecret") {
		t.Errorf("GoString() leaked secret: %s", s)
	}
	if !strings.Contains(s, "REDACTED") {
		t.Errorf("GoString() missing REDACTED marker: %s", s)
	}
}

func TestValidate_NegativeNudgeWindow(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.NudgeWindow = "-1s"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative nudge_window")
	}
}

func TestValidate_EnabledProviderEmptyEvents(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   true,
		SecretEnv: "GT_JIRA_SECRET",
		Events:    []string{}, // empty — should fail
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for enabled provider with empty events list")
	}
}

func TestValidate_DisabledProviderEmptyEventsOK(t *testing.T) {
	t.Parallel()
	cfg := telegraph.DefaultConfig()
	cfg.Telegraph.Providers["jira"] = &telegraph.ProviderConfig{
		Enabled:   false,
		SecretEnv: "GT_JIRA_SECRET",
		Events:    []string{}, // OK when disabled
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for disabled provider with empty events", err)
	}
}
