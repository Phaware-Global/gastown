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
