package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/channelevents"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	emitEventChannel string
	emitEventType    string
	emitEventPayload []string
	emitEventRig     string
)

var moleculeEmitEventCmd = &cobra.Command{
	Use:   "emit-event",
	Short: "Emit a file-based event on a named channel",
	Long: `Emit an event file to ~/gt/events/<channel>/ for subscribers to pick up.

This is the Go counterpart to emit-event.sh. Events are JSON files consumed
by await-event subscribers (e.g., the refinery watching for MERGE_READY events).

EVENT FORMAT:
Creates a JSON file at ~/gt/events/<channel>/<timestamp>.event:
  {"type": "...", "channel": "...", "timestamp": "...", "payload": {...}}

The "refinery" channel is per-rig: the file goes to
~/gt/events/refinery/<rig>/ and the payload carries rig=<rig>. The rig comes
from --payload rig=, else --rig, else GT_RIG, else the current directory.

EXAMPLES:
  # Emit a MERGE_READY event for the refinery
  gt mol step emit-event --channel refinery --type MERGE_READY \
    --payload polecat=nux --payload branch=polecat/nux/gt-iw7m

  # Emit a PATROL_WAKE event
  gt mol step emit-event --channel refinery --rig myrig --type PATROL_WAKE \
    --payload source=witness --payload queue_depth=3

  # Emit an MQ_SUBMIT event
  gt mol step emit-event --channel refinery --type MQ_SUBMIT \
    --payload branch=feat/new-feature --payload mr_id=bd-42`,
	RunE: runMoleculeEmitEvent,
}

// EmitEventResult is returned when an event is emitted.
type EmitEventResult struct {
	Path    string `json:"path"`
	Channel string `json:"channel"`
	Type    string `json:"type"`
}

func init() {
	moleculeEmitEventCmd.Flags().StringVar(&emitEventChannel, "channel", "",
		"Event channel name (required, e.g., 'refinery')")
	moleculeEmitEventCmd.Flags().StringVar(&emitEventType, "type", "",
		"Event type (required, e.g., 'MERGE_READY')")
	moleculeEmitEventCmd.Flags().StringArrayVar(&emitEventPayload, "payload", nil,
		"Payload key=value pairs (repeatable)")
	moleculeEmitEventCmd.Flags().StringVar(&emitEventRig, "rig", "",
		"Target rig on a per-rig channel (default: GT_RIG or cwd)")
	moleculeEmitEventCmd.Flags().BoolVar(&moleculeJSON, "json", false,
		"Output as JSON")
	_ = moleculeEmitEventCmd.MarkFlagRequired("channel")
	_ = moleculeEmitEventCmd.MarkFlagRequired("type")

	moleculeStepCmd.AddCommand(moleculeEmitEventCmd)
}

func runMoleculeEmitEvent(cmd *cobra.Command, args []string) error {
	payload := emitEventPayload
	if channelevents.RigScoped(emitEventChannel) {
		var err error
		if payload, err = rigEventPayload(payload, emitEventRig); err != nil {
			return err
		}
	}
	path, err := channelevents.Emit(emitEventChannel, emitEventType, payload)
	if err != nil {
		return err
	}

	if moleculeJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(EmitEventResult{
			Path:    path,
			Channel: emitEventChannel,
			Type:    emitEventType,
		})
	}

	fmt.Println(path)
	return nil
}

// rigEventPayload makes sure a per-rig channel's payload names its target rig.
func rigEventPayload(payload []string, rigFlag string) ([]string, error) {
	if named := channelevents.Payload(payload)["rig"]; named != "" {
		if rigFlag != "" && rigFlag != named {
			return nil, fmt.Errorf("--rig %s conflicts with --payload rig=%s", rigFlag, named)
		}
		return payload, nil
	}
	townRoot, _ := workspace.FindFromCwd()
	rigName := resolveEventRig(townRoot, rigFlag)
	if rigName == "" {
		return nil, fmt.Errorf("channel %q is per-rig: pass --rig", emitEventChannel)
	}
	return append(slices.Clone(payload), "rig="+rigName), nil
}
