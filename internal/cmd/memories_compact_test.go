package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

	t.Run("truncated at the output limit", func(t *testing.T) {
		// A store too big for one pass stops at max_tokens mid-JSON; the error
		// must name that cause, not the resulting parse failure.
		raw := []byte(`{"is_error":false,"stop_reason":"max_tokens","result":"{\"memories\":[{\"type\":\"user\","}`)
		_, err := parseCompactResponse(raw)
		if err == nil {
			t.Fatal("expected error for a max_tokens-truncated response")
		}
		if !strings.Contains(err.Error(), "output limit") {
			t.Errorf("error = %q, want it to name the output limit", err)
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
		name    string
		use     memoriesFlagUse
		wantErr bool
	}{
		{name: "plain list, no flags", use: memoriesFlagUse{}},
		{name: "plain list with search term", use: memoriesFlagUse{hasArgs: true}},
		{name: "plain list with --type", use: memoriesFlagUse{typeFilter: true}},
		{name: "--dry-run without --compact", use: memoriesFlagUse{dryRun: true}, wantErr: true},
		{name: "--yes without --compact", use: memoriesFlagUse{yes: true}, wantErr: true},
		{name: "--model without --compact", use: memoriesFlagUse{model: true}, wantErr: true},
		{name: "--instructions without --compact", use: memoriesFlagUse{instructions: true}, wantErr: true},
		{name: "--instructions-file without --compact", use: memoriesFlagUse{instructionsFile: true}, wantErr: true},
		{name: "--timeout without --compact", use: memoriesFlagUse{timeout: true, timeoutValue: time.Minute}, wantErr: true},
		{name: "compact alone", use: memoriesFlagUse{compact: true}},
		{
			name: "compact with all its flags",
			use: memoriesFlagUse{compact: true, dryRun: true, yes: true, model: true,
				instructions: true, timeout: true, timeoutValue: 10 * time.Minute},
		},
		{name: "compact with search term", use: memoriesFlagUse{compact: true, hasArgs: true}, wantErr: true},
		{name: "compact with --type", use: memoriesFlagUse{compact: true, typeFilter: true}, wantErr: true},
		{
			name:    "both instruction flags",
			use:     memoriesFlagUse{compact: true, instructions: true, instructionsFile: true},
			wantErr: true,
		},
		{
			name:    "zero --timeout",
			use:     memoriesFlagUse{compact: true, timeout: true, timeoutValue: 0},
			wantErr: true,
		},
		{
			name:    "negative --timeout",
			use:     memoriesFlagUse{compact: true, timeout: true, timeoutValue: -5 * time.Second},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMemoriesFlags(tt.use)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMemoriesFlags() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompactTimeoutFor(t *testing.T) {
	t.Run("scales past the old flat 2m cap for a real store", func(t *testing.T) {
		// The reported failure: 81 memories timed out at a flat 2 minutes.
		got := compactTimeoutFor(81)
		if got <= 2*time.Minute {
			t.Errorf("compactTimeoutFor(81) = %s, want more than the old 2m cap", got)
		}
		if got > compactTimeoutMax {
			t.Errorf("compactTimeoutFor(81) = %s, want <= %s", got, compactTimeoutMax)
		}
	})

	t.Run("monotonic in memory count", func(t *testing.T) {
		if compactTimeoutFor(10) >= compactTimeoutFor(200) {
			t.Error("timeout must grow with the number of memories")
		}
	})

	t.Run("clamped at the max", func(t *testing.T) {
		if got := compactTimeoutFor(100000); got != compactTimeoutMax {
			t.Errorf("compactTimeoutFor(100000) = %s, want %s", got, compactTimeoutMax)
		}
	})

	t.Run("never below the base", func(t *testing.T) {
		for _, n := range []int{-1, 0, 2} {
			if got := compactTimeoutFor(n); got < compactTimeoutBase {
				t.Errorf("compactTimeoutFor(%d) = %s, want >= %s", n, got, compactTimeoutBase)
			}
		}
	})
}

func TestResolveCompactInstructions(t *testing.T) {
	t.Run("no flags means no instructions", func(t *testing.T) {
		got, err := resolveCompactInstructions("", false, "")
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("inline is trimmed", func(t *testing.T) {
		got, err := resolveCompactInstructions("  drop augment notes\n", true, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "drop augment notes" {
			t.Errorf("got %q, want trimmed instructions", got)
		}
	})

	t.Run("blank inline is an error", func(t *testing.T) {
		if _, err := resolveCompactInstructions("   ", true, ""); err == nil {
			t.Error("expected error for whitespace-only --instructions")
		}
	})

	t.Run("file contents are read and trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "instructions.md")
		if err := os.WriteFile(path, []byte("retire the augment workflow\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveCompactInstructions("", false, path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "retire the augment workflow" {
			t.Errorf("got %q, want the file contents", got)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := resolveCompactInstructions("", false, filepath.Join(t.TempDir(), "nope.md")); err == nil {
			t.Error("expected error for unreadable --instructions-file")
		}
	})

	t.Run("empty file is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.md")
		if err := os.WriteFile(path, []byte("\n  \n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveCompactInstructions("", false, path); err == nil {
			t.Error("expected error for an --instructions-file with no instructions")
		}
	})
}

func TestBuildCompactPrompt(t *testing.T) {
	mems := []storedMemory{
		{fullKey: "memory.feedback.augment-loop", memType: "feedback", shortKey: "augment-loop", value: "run augment review"},
	}

	t.Run("without instructions", func(t *testing.T) {
		got := buildCompactPrompt(mems, "")
		if strings.Contains(got, compactInstructionsOpen) {
			t.Error("prompt has an instructions block when none were given")
		}
		if !strings.Contains(got, "feedback/augment-loop") {
			t.Error("prompt is missing the memory list")
		}
	})

	t.Run("with instructions", func(t *testing.T) {
		instructions := "Remove references to augment.\nUse the internal reviewer instead."
		got := buildCompactPrompt(mems, instructions)
		if !strings.Contains(got, compactInstructionsOpen) || !strings.Contains(got, compactInstructionsClose) {
			t.Fatal("instructions are not delimited")
		}
		if !strings.Contains(got, instructions) {
			t.Error("instructions text is missing from the prompt")
		}
		// Multi-line instructions must sit inside the delimiters, not leak into
		// the memory list that follows.
		openIdx := strings.Index(got, compactInstructionsOpen)
		closeIdx := strings.Index(got, compactInstructionsClose)
		if openIdx >= closeIdx || strings.Index(got, "Current memories") < closeIdx {
			t.Error("instructions block is not fully ahead of the memory list")
		}
		// The model must be told the instructions outrank the default goals,
		// otherwise "preserve every distinct fact" blocks a requested removal.
		if !strings.Contains(got, "outrank") {
			t.Error("prompt does not establish instruction precedence over the goals")
		}
	})
}

func TestCompactClaudeArgs(t *testing.T) {
	args := compactClaudeArgs("sonnet")

	// A turn budget of 1 aborts the whole run (error_max_turns) the moment the
	// model attempts a tool call, which it may do even when tools are denied.
	var turns string
	for i, a := range args {
		if a == "--max-turns" && i+1 < len(args) {
			turns = args[i+1]
		}
	}
	if turns == "" {
		t.Fatal("--max-turns is not set")
	}
	if n, err := strconv.Atoi(turns); err != nil || n < 2 {
		t.Errorf("--max-turns = %q, want an integer >= 2", turns)
	}

	if args[len(args)-1] != "-p" {
		t.Errorf("last arg = %q, want -p to terminate the variadic tool list", args[len(args)-1])
	}

	// Compaction needs no tools, and it runs with permissions skipped inside the
	// user's town directory — the mutating tools must be denied.
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--disallowed-tools") {
		t.Fatal("--disallowed-tools is not set")
	}
	for _, tool := range []string{"Bash", "Write", "Edit"} {
		if !slices.Contains(compactDeniedTools, tool) {
			t.Errorf("%s is not denied for the compaction call", tool)
		}
	}
}

func TestBuildCompactPromptForbidsTools(t *testing.T) {
	// The prompt must tell the model not to reach for tools: a tool call costs a
	// turn and buys nothing, since every memory is already in the prompt.
	got := buildCompactPrompt([]storedMemory{
		{fullKey: "memory.user.k", memType: "user", shortKey: "k", value: "v"},
	}, "")
	if !strings.Contains(got, "Do not use any tools") {
		t.Error("prompt does not tell the model to avoid tools")
	}
}

func TestClaudeCompactError(t *testing.T) {
	baseErr := errors.New("exit status 1")

	t.Run("surfaces the stdout error envelope", func(t *testing.T) {
		// The real failure mode: claude reports error_max_turns on STDOUT and
		// leaves stderr empty, so a bare ExitError says only "exit status 1".
		stdout := []byte(`{"is_error":true,"subtype":"error_max_turns","result":""}`)
		err := claudeCompactError(baseErr, stdout)
		if !strings.Contains(err.Error(), "error_max_turns") {
			t.Errorf("error = %q, want it to name the subtype from stdout", err)
		}
	})

	t.Run("falls back to raw stdout", func(t *testing.T) {
		err := claudeCompactError(baseErr, []byte("something went wrong"))
		if !strings.Contains(err.Error(), "something went wrong") {
			t.Errorf("error = %q, want it to include raw stdout", err)
		}
	})

	t.Run("passes the error through when there is nothing to add", func(t *testing.T) {
		if err := claudeCompactError(baseErr, nil); !errors.Is(err, baseErr) {
			t.Errorf("error = %v, want the original error", err)
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

	t.Run("rejects missing type (no silent re-typing)", func(t *testing.T) {
		result := &memCompactResult{Memories: []compactMemory{{Type: "", Key: "k", Value: "v"}}}
		if _, err := buildCompactPlan(originals, result); err == nil {
			t.Fatal("expected error when model omits the type field")
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
