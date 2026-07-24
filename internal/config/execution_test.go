package config

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestExecutionConfig_NilSafeDefaults(t *testing.T) {
	var e *ExecutionConfig
	if got := e.BackendName(); got != ExecutionBackendLocal {
		t.Errorf("BackendName() = %q, want %q", got, ExecutionBackendLocal)
	}
	if e.IsRemote() {
		t.Error("IsRemote() = true for nil config, want false")
	}
	if got := e.NetworkMode(); got != NetworkModeOpen {
		t.Errorf("NetworkMode() = %q, want %q", got, NetworkModeOpen)
	}
	if got := e.CheckpointInterval(); got != DefaultCheckpointInterval {
		t.Errorf("CheckpointInterval() = %v, want %v", got, DefaultCheckpointInterval)
	}
	if got := e.Cooldown(); got != DefaultExecutionCooldown {
		t.Errorf("Cooldown() = %v, want %v", got, DefaultExecutionCooldown)
	}
	if got := e.MaxRuntime(); got != DefaultMaxRuntime {
		t.Errorf("MaxRuntime() = %v, want %v", got, DefaultMaxRuntime)
	}
	if e.ProviderExtension() != nil {
		t.Error("ProviderExtension() != nil for nil config")
	}
}

func TestExecutionConfig_UnmarshalPreservesProviderExtension(t *testing.T) {
	raw := `{
		"backend": "ec2",
		"exec_mode": "container",
		"image": "example.com/dev:latest",
		"requires_docker": true,
		"network": {"mode": "gateway"},
		"checkpoint_interval": "2m",
		"cooldown": "1m",
		"max_runtime": "8h",
		"ec2": {"region": "us-east-1", "instance_type": "c7i.2xlarge"},
		"socket": {"address": "10.0.0.5:9878"}
	}`
	var e ExecutionConfig
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if e.Backend != "ec2" || !e.IsRemote() {
		t.Errorf("Backend = %q, IsRemote = %v", e.Backend, e.IsRemote())
	}
	if e.ExecMode != ExecModeContainer || e.Image != "example.com/dev:latest" || !e.RequiresDocker {
		t.Errorf("shared fields wrong: %+v", e)
	}
	if e.NetworkMode() != NetworkModeGateway {
		t.Errorf("NetworkMode() = %q", e.NetworkMode())
	}
	if e.CheckpointInterval() != 2*time.Minute || e.Cooldown() != time.Minute || e.MaxRuntime() != 8*time.Hour {
		t.Errorf("durations wrong: %v %v %v", e.CheckpointInterval(), e.Cooldown(), e.MaxRuntime())
	}

	ext := e.ProviderExtension()
	if ext == nil {
		t.Fatal("ProviderExtension() = nil, want ec2 object")
	}
	var ec2 struct {
		Region       string `json:"region"`
		InstanceType string `json:"instance_type"`
	}
	if err := json.Unmarshal(ext, &ec2); err != nil {
		t.Fatalf("unmarshal extension: %v", err)
	}
	if ec2.Region != "us-east-1" || ec2.InstanceType != "c7i.2xlarge" {
		t.Errorf("extension = %+v", ec2)
	}
	// The non-selected provider's block is preserved but not selected.
	if _, ok := e.Extensions["socket"]; !ok {
		t.Error("socket extension not preserved")
	}
}

func TestExecutionConfig_MarshalRoundTrip(t *testing.T) {
	in := `{"backend":"ec2","exec_mode":"native","ec2":{"region":"eu-west-1"}}`
	var e ExecutionConfig
	if err := json.Unmarshal([]byte(in), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := json.Marshal(&e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var e2 ExecutionConfig
	if err := json.Unmarshal(out, &e2); err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if e2.Backend != "ec2" || e2.ExecMode != ExecModeNative {
		t.Errorf("round trip lost shared fields: %+v", e2)
	}
	if string(e2.ProviderExtension()) != `{"region":"eu-west-1"}` {
		t.Errorf("round trip lost extension: %s", e2.ProviderExtension())
	}
}

func TestValidateExecutionConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ExecutionConfig
		wantErr bool
	}{
		{"empty is valid", ExecutionConfig{}, false},
		{"local backend", ExecutionConfig{Backend: "local"}, false},
		{"container mode", ExecutionConfig{ExecMode: ExecModeContainer}, false},
		{"native mode", ExecutionConfig{ExecMode: ExecModeNative}, false},
		{"bad exec_mode", ExecutionConfig{ExecMode: "vm"}, true},
		{"gateway network", ExecutionConfig{Network: &ExecutionNetworkConfig{Mode: NetworkModeGateway}}, false},
		{"bad network mode", ExecutionConfig{Network: &ExecutionNetworkConfig{Mode: "airgapped"}}, true},
		{"valid durations", ExecutionConfig{CheckpointStr: "30s", CooldownStr: "5m", MaxRuntimeStr: "12h"}, false},
		{"bad duration", ExecutionConfig{MaxRuntimeStr: "4 hours"}, true},
		{"negative duration", ExecutionConfig{CooldownStr: "-5m"}, true},
		{"zero duration", ExecutionConfig{CheckpointStr: "0s"}, true},
		{"docker+sandboxed rejected", ExecutionConfig{
			RequiresDocker: true,
			Network:        &ExecutionNetworkConfig{Mode: NetworkModeSandboxed},
		}, true},
		{"docker+gateway ok", ExecutionConfig{
			RequiresDocker: true,
			Network:        &ExecutionNetworkConfig{Mode: NetworkModeGateway},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExecutionConfig(&tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateExecutionConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidExecutionConfig) {
				t.Errorf("error not wrapped in ErrInvalidExecutionConfig: %v", err)
			}
		})
	}
}

func TestValidateRigSettings_ExecutionBlock(t *testing.T) {
	s := &RigSettings{
		Type:      "rig-settings",
		Version:   CurrentRigSettingsVersion,
		Execution: &ExecutionConfig{ExecMode: "bogus"},
	}
	if err := validateRigSettings(s); err == nil {
		t.Error("validateRigSettings accepted invalid execution block")
	}
	s.Execution = &ExecutionConfig{Backend: "ec2", ExecMode: ExecModeContainer}
	if err := validateRigSettings(s); err != nil {
		t.Errorf("validateRigSettings rejected valid execution block: %v", err)
	}
}
