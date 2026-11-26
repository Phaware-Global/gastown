// Package channelevents provides file-based event emission for named channels.
//
// Channel events are JSON files written to ~/gt/events/<channel>/*.event
// (~/gt/events/<channel>/<rig>/*.event for rig-scoped channels) and consumed
// by await-event subscribers (e.g., the refinery watching for MERGE_READY
// events). This is distinct from the activity feed events in the events
// package (~/gt/.events.jsonl).
package channelevents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/steveyegge/gastown/internal/workspace"
)

// ValidChannelName restricts channel names to safe characters (no path traversal).
var ValidChannelName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// RigScoped reports whether every rig has its own consumer of the channel, in
// which case events are partitioned by rig so that one rig cannot consume (and
// delete) another's.
func RigScoped(channel string) bool {
	return channel == "refinery"
}

// Dir returns the directory holding a channel's events. rig is required for
// rig-scoped channels and ignored for the rest.
func Dir(townRoot, channel, rig string) (string, error) {
	if !ValidChannelName.MatchString(channel) {
		return "", fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}
	dir := filepath.Join(townRoot, "events", channel)
	if !RigScoped(channel) {
		return dir, nil
	}
	if !ValidChannelName.MatchString(rig) {
		return "", fmt.Errorf("channel %q is per-rig: invalid or missing rig name %q", channel, rig)
	}
	return filepath.Join(dir, rig), nil
}

// emitSeq is an atomic counter to ensure unique event filenames even when
// time.Now().UnixNano() has low resolution.
var emitSeq atomic.Uint64

// Emit creates an event file in the channel directory, resolving the town
// root from the current working directory. Rig-scoped channels take the rig
// from the "rig=" payload pair.
func Emit(channel, eventType string, payloadPairs []string) (string, error) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		home, _ := os.UserHomeDir()
		townRoot = filepath.Join(home, "gt")
	}
	return EmitToTown(townRoot, channel, eventType, payloadPairs)
}

// EmitToTown creates an event file using an explicit town root.
// Used by internal callers that already know the town root.
func EmitToTown(townRoot, channel, eventType string, payloadPairs []string) (string, error) {
	payload := Payload(payloadPairs)
	eventDir, err := Dir(townRoot, channel, payload["rig"])
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(eventDir, 0755); err != nil {
		return "", fmt.Errorf("creating event directory: %w", err)
	}
	return emitToDir(eventDir, channel, eventType, payload)
}

// Payload folds key=value pairs into a map. Pairs without "=" are dropped and
// the last value for a key wins.
func Payload(pairs []string) map[string]string {
	payload := make(map[string]string)
	for _, pair := range pairs {
		key, val, found := strings.Cut(pair, "=")
		if found {
			payload[key] = val
		}
	}
	return payload
}

// emitToDir writes an event file to the given directory.
func emitToDir(eventDir, channel, eventType string, payload map[string]string) (string, error) {
	if !ValidChannelName.MatchString(channel) {
		return "", fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}

	now := time.Now()
	event := map[string]interface{}{
		"type":      eventType,
		"channel":   channel,
		"timestamp": now.Format(time.RFC3339),
		"payload":   payload,
	}

	data, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling event: %w", err)
	}

	seq := emitSeq.Add(1)
	eventFile := filepath.Join(eventDir, fmt.Sprintf("%d-%d-%d.event", now.UnixNano(), seq, os.Getpid()))
	if err := os.WriteFile(eventFile, data, 0644); err != nil {
		return "", fmt.Errorf("writing event file: %w", err)
	}

	return eventFile, nil
}
