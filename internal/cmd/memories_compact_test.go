package cmd

import (
	"testing"
)

func TestExtractJSONSpan(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"prose prefix and suffix", "Sure!\n{\"a\":1}\nDone", `{"a":1}`},
		{"nested braces", `prefix {"a":{"b":2}} suffix`, `{"a":{"b":2}}`},
		{"no object", "no json here", ""},
		{"only open brace", "text { more", ""},
		{"json fenced block", "Here:\n```json\n{\"a\":1}\n```\nthanks", `{"a":1}`},
		{"bare fenced block", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fence with prose containing braces", "I think {x} maybe.\n```json\n{\"a\":1}\n```", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractJSONSpan(tt.in); got != tt.want {
				t.Errorf("extractJSONSpan(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseCompactResponse(t *testing.T) {
	t.Run("claude envelope with embedded JSON", func(t *testing.T) {
		raw := []byte(`{"type":"result","is_error":false,"result":"Here you go:\n{\"memories\":[{\"type\":\"feedback\",\"key\":\"k\",\"value\":\"v\",\"sources\":[\"k\"]}],\"dropped\":[]}"}`)
		got, err := parseCompactResponse(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Memories) != 1 || got.Memories[0].Key != "k" {
			t.Fatalf("got %+v, want one memory with key k", got.Memories)
		}
	})

	t.Run("bare JSON without envelope", func(t *testing.T) {
		raw := []byte(`{"memories":[{"type":"user","key":"u","value":"x"}],"dropped":[]}`)
		got, err := parseCompactResponse(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Memories) != 1 || got.Memories[0].Type != "user" {
			t.Fatalf("got %+v", got.Memories)
		}
	})

	t.Run("model error envelope", func(t *testing.T) {
		raw := []byte(`{"is_error":true,"subtype":"error_max_turns","result":"ran out"}`)
		if _, err := parseCompactResponse(raw); err == nil {
			t.Fatal("expected error for is_error envelope")
		}
	})

	t.Run("no JSON object", func(t *testing.T) {
		raw := []byte(`{"result":"I cannot help with that"}`)
		if _, err := parseCompactResponse(raw); err == nil {
			t.Fatal("expected error when no JSON object embedded")
		}
	})
}

func TestValidateMemoriesFlags(t *testing.T) {
	tests := []struct {
		name                                           string
		compact, hasArgs, dryRun, yes, model, typeFlag bool
		wantErr                                        bool
	}{
		{name: "plain list, no flags", wantErr: false},
		{name: "plain list with search term", hasArgs: true, wantErr: false},
		{name: "plain list with --type", typeFlag: true, wantErr: false},
		{name: "--dry-run without --compact", dryRun: true, wantErr: true},
		{name: "--yes without --compact", yes: true, wantErr: true},
		{name: "--model without --compact", model: true, wantErr: true},
		{name: "compact alone", compact: true, wantErr: false},
		{name: "compact with all its flags", compact: true, dryRun: true, yes: true, model: true, wantErr: false},
		{name: "compact with search term", compact: true, hasArgs: true, wantErr: true},
		{name: "compact with --type", compact: true, typeFlag: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMemoriesFlags(tt.compact, tt.hasArgs, tt.dryRun, tt.yes, tt.model, tt.typeFlag)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMemoriesFlags() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildCompactPlan(t *testing.T) {
	originals := []storedMemory{
		{fullKey: "memory.feedback.pr-target-fork", memType: "feedback", shortKey: "pr-target-fork", value: "target fork"},
		{fullKey: "memory.feedback.augment-loop", memType: "feedback", shortKey: "augment-loop", value: "run augment review"},
		{fullKey: "memory.user.senior-go", memType: "user", shortKey: "senior-go", value: "senior go dev"},
		{fullKey: "memory.stale-legacy", memType: "general", shortKey: "stale-legacy", value: "old fact"}, // legacy untyped key
	}

	t.Run("merge two, keep one, drop legacy", func(t *testing.T) {
		result := &memCompactResult{
			Memories: []compactMemory{
				{Type: "feedback", Key: "pr-review-workflow", Value: "target fork; run augment review",
					Sources: []string{"pr-target-fork", "augment-loop"}},
				{Type: "user", Key: "senior-go", Value: "senior go dev", Sources: []string{"senior-go"}},
			},
			Dropped: []compactDrop{{Key: "stale-legacy", Reason: "obsolete"}},
		}
		plan, err := buildCompactPlan(originals, result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// New merged memory + unchanged user memory.
		if plan.writes() != 1 {
			t.Errorf("writes() = %d, want 1 (only the merged memory is new)", plan.writes())
		}
		// Clears: the two merged sources + the dropped legacy key.
		if len(plan.clears) != 3 {
			t.Fatalf("clears = %d, want 3 (%v)", len(plan.clears), plan.clears)
		}
		// The unchanged user memory must NOT be cleared.
		for _, c := range plan.clears {
			if c.fullKey == "memory.user.senior-go" {
				t.Error("unchanged user memory was scheduled for deletion")
			}
		}
		// Drop reason maps onto the legacy key.
		if plan.dropReasons["memory.stale-legacy"] != "obsolete" {
			t.Errorf("drop reason for legacy key = %q, want obsolete", plan.dropReasons["memory.stale-legacy"])
		}
	})

	t.Run("unchanged legacy key is preserved, not duplicated", func(t *testing.T) {
		// Model returns the legacy general memory unchanged. It must reuse the
		// existing legacy fullKey (memory.stale-legacy) rather than create
		// memory.general.stale-legacy, so it is neither written nor cleared.
		result := &memCompactResult{
			Memories: []compactMemory{
				{Type: "general", Key: "stale-legacy", Value: "old fact", Sources: []string{"stale-legacy"}},
			},
		}
		plan, err := buildCompactPlan(originals, result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, s := range plan.sets {
			if s.fullKey == "memory.general.stale-legacy" {
				t.Error("legacy memory rewritten under a new typed key (would duplicate)")
			}
			if s.fullKey == "memory.stale-legacy" && (s.isNew || s.changed) {
				t.Error("unchanged legacy memory marked new/changed")
			}
		}
	})

	t.Run("duplicate type/key prefers canonical key, clears legacy", func(t *testing.T) {
		// Both a legacy untyped key and the canonical typed key resolve to
		// general/dup. The canonical one must be preserved and the legacy one
		// cleared, deterministically (not order-dependent).
		dupOriginals := []storedMemory{
			{fullKey: "memory.dup", memType: "general", shortKey: "dup", value: "same fact"},
			{fullKey: "memory.general.dup", memType: "general", shortKey: "dup", value: "same fact"},
		}
		result := &memCompactResult{
			Memories: []compactMemory{
				{Type: "general", Key: "dup", Value: "same fact", Sources: []string{"general/dup"}},
			},
		}
		plan, err := buildCompactPlan(dupOriginals, result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(plan.sets) != 1 || plan.sets[0].fullKey != "memory.general.dup" {
			t.Fatalf("expected single set op on canonical key memory.general.dup, got %+v", plan.sets)
		}
		if len(plan.clears) != 1 || plan.clears[0].fullKey != "memory.dup" {
			t.Fatalf("expected to clear the legacy key memory.dup, got %+v", plan.clears)
		}
	})

	t.Run("refuses empty memory set", func(t *testing.T) {
		if _, err := buildCompactPlan(originals, &memCompactResult{Memories: nil}); err == nil {
			t.Fatal("expected refusal when model returns no memories")
		}
	})

	t.Run("rejects invalid type", func(t *testing.T) {
		result := &memCompactResult{Memories: []compactMemory{{Type: "bogus", Key: "k", Value: "v"}}}
		if _, err := buildCompactPlan(originals, result); err == nil {
			t.Fatal("expected error for invalid memory type")
		}
	})

	t.Run("rejects empty key", func(t *testing.T) {
		result := &memCompactResult{Memories: []compactMemory{{Type: "user", Key: "  ", Value: "v"}}}
		if _, err := buildCompactPlan(originals, result); err == nil {
			t.Fatal("expected error for empty key")
		}
	})

	t.Run("rejects empty value", func(t *testing.T) {
		result := &memCompactResult{Memories: []compactMemory{{Type: "user", Key: "k", Value: "   "}}}
		if _, err := buildCompactPlan(originals, result); err == nil {
			t.Fatal("expected error for empty value")
		}
	})

	t.Run("rejects duplicate final key", func(t *testing.T) {
		result := &memCompactResult{Memories: []compactMemory{
			{Type: "user", Key: "dup", Value: "a"},
			{Type: "user", Key: "dup", Value: "b"},
		}}
		if _, err := buildCompactPlan(originals, result); err == nil {
			t.Fatal("expected error for duplicate final key")
		}
	})

	t.Run("detects changed value on existing key", func(t *testing.T) {
		result := &memCompactResult{Memories: []compactMemory{
			{Type: "user", Key: "senior-go", Value: "senior go dev, 10y", Sources: []string{"senior-go"}},
		}}
		plan, err := buildCompactPlan(originals, result)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var found bool
		for _, s := range plan.sets {
			if s.fullKey == "memory.user.senior-go" {
				found = true
				if s.isNew || !s.changed {
					t.Errorf("existing key with new value: isNew=%v changed=%v, want isNew=false changed=true", s.isNew, s.changed)
				}
			}
		}
		if !found {
			t.Fatal("expected a set op for memory.user.senior-go")
		}
	})
}
