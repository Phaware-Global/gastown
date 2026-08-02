package polecat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/execution"
)

// fakeBackend records calls and returns a canned launcher argv.
type fakeBackend struct {
	provisioned *execution.PolecatSpec
	wrapCalls   int
	gotArgv     []string
	gotEnv      map[string]string
	launcher    []string
}

func (f *fakeBackend) Provision(_ context.Context, spec execution.PolecatSpec) (execution.Endpoint, error) {
	f.provisioned = &spec
	return execution.Endpoint{Backend: "faketest", ID: "worker-1", Identity: spec.Identity()}, nil
}

func (f *fakeBackend) WrapCommand(_ execution.Endpoint, argv []string, env map[string]string) ([]string, error) {
	f.wrapCalls++
	f.gotArgv = argv
	f.gotEnv = env
	return f.launcher, nil
}

func (f *fakeBackend) Teardown(context.Context, execution.Endpoint) error { return nil }
func (f *fakeBackend) Discover(context.Context, execution.IdentityTags) ([]execution.Endpoint, error) {
	return nil, nil
}

func TestBuildRemoteArgv(t *testing.T) {
	fake := &fakeBackend{launcher: []string{"fake-launcher", "--session", "s1", "--", "claude", "hi there"}}
	execution.Register("faketest", func(*config.ExecutionConfig) (execution.Backend, error) {
		return fake, nil
	})

	execCfg := &config.ExecutionConfig{Backend: "faketest"}
	spec := execution.PolecatSpec{Rig: "myr", Polecat: "mycat", Session: "myr-mycat", Config: execCfg}
	agentArgv := []string{"claude", "--model", "opus", "do work"}
	env := map[string]string{"GT_ROLE": "myr/polecats/mycat", "GT_PROXY_URL": "http://127.0.0.1:9899"}

	prov, err := buildRemoteArgv(context.Background(), execCfg, spec, agentArgv, env)
	if err != nil {
		t.Fatalf("buildRemoteArgv: %v", err)
	}

	if fake.provisioned == nil {
		t.Fatal("Provision was not called")
	}
	if fake.provisioned.Rig != "myr" || fake.provisioned.Polecat != "mycat" {
		t.Errorf("Provision spec = %+v", fake.provisioned)
	}
	if fake.wrapCalls != 1 {
		t.Errorf("WrapCommand called %d times, want 1", fake.wrapCalls)
	}
	if !reflect.DeepEqual(fake.gotArgv, agentArgv) {
		t.Errorf("backend received argv %v, want %v", fake.gotArgv, agentArgv)
	}
	if fake.gotEnv["GT_PROXY_URL"] != "http://127.0.0.1:9899" {
		t.Errorf("backend received env %v", fake.gotEnv)
	}
	if !reflect.DeepEqual(prov.argv, fake.launcher) {
		t.Errorf("wrapped = %v, want backend launcher %v", prov.argv, fake.launcher)
	}
}

func TestBuildRemoteArgv_UnknownBackend(t *testing.T) {
	execCfg := &config.ExecutionConfig{Backend: "no-such-provider"}
	_, err := buildRemoteArgv(context.Background(), execCfg, execution.PolecatSpec{}, []string{"claude"}, nil)
	if err == nil {
		t.Fatal("expected error for unregistered backend")
	}
}

// tearDownBackend counts Teardown calls and can fail WrapCommand on demand.
type tearDownBackend struct {
	failWrap     bool
	teardownCnt  int
	provisionCnt int
}

func (b *tearDownBackend) Provision(_ context.Context, spec execution.PolecatSpec) (execution.Endpoint, error) {
	b.provisionCnt++
	return execution.Endpoint{Backend: "teardowntest", ID: "w1", Identity: spec.Identity()}, nil
}

func (b *tearDownBackend) WrapCommand(_ execution.Endpoint, argv []string, _ map[string]string) ([]string, error) {
	if b.failWrap {
		return nil, fmt.Errorf("wrap boom")
	}
	return append([]string{"launch"}, argv...), nil
}

func (b *tearDownBackend) Teardown(context.Context, execution.Endpoint) error {
	b.teardownCnt++
	return nil
}

func (b *tearDownBackend) Discover(context.Context, execution.IdentityTags) ([]execution.Endpoint, error) {
	return nil, nil
}

// TestBuildRemoteArgv_TeardownOnWrapFailure verifies that a WrapCommand failure
// after a successful Provision releases the worker rather than leaking it.
func TestBuildRemoteArgv_TeardownOnWrapFailure(t *testing.T) {
	backend := &tearDownBackend{failWrap: true}
	execution.Register("teardowntest", func(*config.ExecutionConfig) (execution.Backend, error) {
		return backend, nil
	})
	execCfg := &config.ExecutionConfig{Backend: "teardowntest"}

	_, err := buildRemoteArgv(context.Background(), execCfg,
		execution.PolecatSpec{Rig: "r", Polecat: "p"}, []string{"claude"}, nil)
	if err == nil {
		t.Fatal("expected wrap failure")
	}
	if backend.provisionCnt != 1 {
		t.Errorf("provisionCnt = %d, want 1", backend.provisionCnt)
	}
	if backend.teardownCnt != 1 {
		t.Errorf("teardownCnt = %d, want 1 (worker leaked on wrap failure)", backend.teardownCnt)
	}
}

// TestRemoteProvision_TeardownOnStartFailure verifies the deferred cleanup
// contract: teardown() releases the provisioned worker (the path Start's
// defer runs when a later start step errors).
func TestRemoteProvision_TeardownOnStartFailure(t *testing.T) {
	backend := &tearDownBackend{}
	prov := &remoteProvision{
		argv:    []string{"launch", "claude"},
		backend: backend,
		ep:      execution.Endpoint{ID: "w1"},
	}
	prov.teardown()
	if backend.teardownCnt != 1 {
		t.Errorf("teardownCnt = %d, want 1", backend.teardownCnt)
	}
	// Nil-safe.
	var nilProv *remoteProvision
	nilProv.teardown()
}

func TestShellJoinArgv(t *testing.T) {
	got := shellJoinArgv([]string{"fake-launcher", "--flag", "value with spaces", "plain"})
	want := "fake-launcher --flag 'value with spaces' plain"
	if got != want {
		t.Errorf("shellJoinArgv = %q, want %q", got, want)
	}
}

func TestRigExecutionConfig(t *testing.T) {
	rigPath := t.TempDir()

	// No settings file → nil (local).
	if cfg := rigExecutionConfig(rigPath); cfg != nil {
		t.Errorf("no settings: got %+v, want nil", cfg)
	}
	if rigExecutionConfig(rigPath).IsRemote() {
		t.Error("nil execution config must be local")
	}

	// Settings with a remote execution block.
	settingsDir := filepath.Join(rigPath, "settings")
	_ = os.MkdirAll(settingsDir, 0755)
	s := &config.RigSettings{
		Type: "rig-settings", Version: config.CurrentRigSettingsVersion,
		Execution: &config.ExecutionConfig{Backend: "faketest"},
	}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	cfg := rigExecutionConfig(rigPath)
	if cfg == nil || !cfg.IsRemote() || cfg.BackendName() != "faketest" {
		t.Errorf("got %+v, want remote faketest", cfg)
	}
}
