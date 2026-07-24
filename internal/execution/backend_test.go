package execution

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestForConfig_NilAndLocal(t *testing.T) {
	for _, cfg := range []*config.ExecutionConfig{nil, {}, {Backend: "local"}} {
		b, err := ForConfig(cfg)
		if err != nil {
			t.Fatalf("ForConfig(%+v): %v", cfg, err)
		}
		if _, ok := b.(LocalBackend); !ok {
			t.Fatalf("ForConfig(%+v) = %T, want LocalBackend", cfg, b)
		}
	}
}

func TestForConfig_UnknownBackend(t *testing.T) {
	_, err := ForConfig(&config.ExecutionConfig{Backend: "ec2"})
	if err == nil {
		t.Fatal("ForConfig(ec2) succeeded with no registered ec2 backend")
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register(config.ExecutionBackendLocal, func(*config.ExecutionConfig) (Backend, error) {
		return LocalBackend{}, nil
	})
}

func TestLocalBackend_NoBehaviorChange(t *testing.T) {
	b := LocalBackend{}
	spec := PolecatSpec{Rig: "gastown", Polecat: "furiosa", Session: "s1"}

	ep, err := b.Provision(context.Background(), spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if ep.Backend != "local" || ep.Identity.Rig != "gastown" || ep.Identity.Polecat != "furiosa" {
		t.Errorf("Provision endpoint = %+v", ep)
	}

	argv := []string{"claude", "--model", "claude-opus-4-8", "do the thing"}
	got, err := b.WrapCommand(ep, argv, map[string]string{"GT_ROLE": "x"})
	if err != nil {
		t.Fatalf("WrapCommand: %v", err)
	}
	if len(got) != len(argv) {
		t.Fatalf("WrapCommand changed argv: %v", got)
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Fatalf("WrapCommand changed argv[%d]: %q != %q", i, got[i], argv[i])
		}
	}

	if err := b.Teardown(context.Background(), ep); err != nil {
		t.Errorf("Teardown: %v", err)
	}
	eps, err := b.Discover(context.Background(), IdentityTags{})
	if err != nil || eps != nil {
		t.Errorf("Discover = %v, %v", eps, err)
	}
}

func TestPolecatSpec_ProviderExtensionFlows(t *testing.T) {
	raw := `{"backend":"local","checkpoint_interval":"1m","local":{"note":"ignored"}}`
	var cfg config.ExecutionConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	b, err := ForConfig(&cfg)
	if err != nil {
		t.Fatalf("ForConfig: %v", err)
	}
	if _, ok := b.(LocalBackend); !ok {
		t.Fatalf("got %T", b)
	}
}
