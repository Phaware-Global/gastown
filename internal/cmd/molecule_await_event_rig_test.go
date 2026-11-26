package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/channelevents"
)

// Two rigs share the "refinery" channel name; a consumer with --cleanup must
// see and delete only its own rig's events.
func TestAwaitEventRefineryChannelIsRigScoped(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(town)
	t.Setenv("GT_RIG", "alpha")

	emit := func(rig string) string {
		path, err := channelevents.EmitToTown(town, "refinery", "MQ_SUBMIT", []string{"rig=" + rig})
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	foreign := emit("beta")
	own := emit("alpha")

	oldChannel, oldCleanup, oldQuiet, oldTimeout := awaitEventChannel, awaitEventCleanup, awaitEventQuiet, awaitEventTimeout
	t.Cleanup(func() {
		awaitEventChannel, awaitEventCleanup, awaitEventQuiet, awaitEventTimeout = oldChannel, oldCleanup, oldQuiet, oldTimeout
	})
	awaitEventChannel, awaitEventCleanup, awaitEventQuiet, awaitEventTimeout = "refinery", true, true, "100ms"

	if err := runMoleculeAwaitEvent(moleculeAwaitEventCmd, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("rig alpha consumed rig beta's event: %v", err)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("rig alpha did not consume its own event %s (err=%v)", strings.TrimPrefix(own, town), err)
	}
}

func TestRigEventPayload(t *testing.T) {
	t.Setenv("GT_RIG", "alpha")
	for _, tt := range []struct {
		name    string
		payload []string
		flag    string
		want    string
		wantErr bool
	}{
		{name: "env default", want: "alpha"},
		{name: "flag beats env", flag: "beta", want: "beta"},
		{name: "payload beats env", payload: []string{"rig=gamma"}, want: "gamma"},
		{name: "flag conflicts with payload", payload: []string{"rig=gamma"}, flag: "beta", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rigEventPayload(tt.payload, tt.flag)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if rig := channelevents.Payload(got)["rig"]; rig != tt.want {
				t.Errorf("rig = %q, want %q", rig, tt.want)
			}
		})
	}
}
