package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/style"
)

// compactTimeout bounds the headless compaction model call so a hung or
// slow `claude` invocation can't stall the command indefinitely.
var compactTimeout = 2 * time.Minute

// memCompactResult is the JSON contract the LLM must return: the complete
// desired final memory set plus, for display only, the list of memories it
// dropped and why. Apply logic never trusts this bookkeeping for deletions —
// it computes removals as (original set − final set) so a hallucinated or
// omitted "dropped" entry cannot silently delete a memory the model still
// listed under "memories".
type memCompactResult struct {
	Memories []compactMemory `json:"memories"`
	Dropped  []compactDrop   `json:"dropped"`
}

type compactMemory struct {
	Type    string   `json:"type"`
	Key     string   `json:"key"`
	Value   string   `json:"value"`
	Sources []string `json:"sources"`
}

type compactDrop struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// storedMemory is one memory.* entry as it currently lives in the kv store.
// fullKey is the exact kv key (which may be a legacy untyped memory.<key>),
// preserved so we clear the right key on apply.
type storedMemory struct {
	fullKey  string
	memType  string
	shortKey string
	value    string
}

// memSetOp is a memory the plan will write.
type memSetOp struct {
	fullKey  string
	memType  string
	shortKey string
	value    string
	sources  []string
	isNew    bool // no original entry at this key
	changed  bool // original entry exists but value differs
}

// compactPlan is the resolved, deterministic set of writes and deletes.
type compactPlan struct {
	sets        []memSetOp
	clears      []storedMemory
	dropReasons map[string]string // fullKey -> reason (display only)
}

func (p *compactPlan) writes() int {
	n := 0
	for _, s := range p.sets {
		if s.isNew || s.changed {
			n++
		}
	}
	return n
}

// runMemoriesCompact implements `gt memories --compact`: load the current
// memories, ask an LLM to consolidate them, show the plan, and (unless
// --dry-run) apply after confirmation.
func runMemoriesCompact() error {
	originals, err := loadStoredMemories()
	if err != nil {
		return fmt.Errorf("loading memories: %w", err)
	}

	if len(originals) < 2 {
		fmt.Printf("%s Nothing to compact (%d memor%s stored).\n",
			style.Dim.Render("ℹ"), len(originals), plural(len(originals)))
		return nil
	}

	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("claude binary not found on PATH — required for LLM-assisted compaction")
	}

	fmt.Printf("%s Compacting %d memories with %s...\n\n",
		style.Bold.Render("🧹"), len(originals), style.Bold.Render(memoriesModel))

	ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
	defer cancel()
	raw, err := invokeClaudeCompact(ctx, buildCompactPrompt(originals), memoriesModel)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("compaction model timed out after %s", compactTimeout)
		}
		return fmt.Errorf("invoking compaction model: %w", err)
	}

	result, err := parseCompactResponse(raw)
	if err != nil {
		return fmt.Errorf("parsing compaction response: %w", err)
	}

	plan, err := buildCompactPlan(originals, result)
	if err != nil {
		return err
	}

	if plan.writes() == 0 && len(plan.clears) == 0 {
		fmt.Printf("%s Memories are already compact — no changes proposed.\n", style.Success.Render("✓"))
		return nil
	}

	renderCompactPlan(originals, plan)

	if memoriesDryRun {
		fmt.Printf("\n%s Dry run — no changes written. Re-run without --dry-run to apply.\n", style.Dim.Render("ℹ"))
		return nil
	}

	if !memoriesYes {
		fmt.Printf("\nApply this plan? [y/N] ")
		// Read the whole line (not fmt.Scanln, which stops at the first space
		// and leaves the rest in the stdin buffer) so trailing input can't
		// bleed into a later prompt.
		response, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(response)) {
		case "y", "yes":
		default:
			fmt.Println("Aborted — no changes written.")
			return nil
		}
	}

	return applyCompactPlan(plan)
}

// loadStoredMemories reads all memory.* entries from the kv store.
func loadStoredMemories() ([]storedMemory, error) {
	kvs, err := bdKvListJSON()
	if err != nil {
		return nil, err
	}
	var mems []storedMemory
	for k, v := range kvs {
		if !strings.HasPrefix(k, memoryKeyPrefix) {
			continue
		}
		memType, shortKey := parseMemoryKey(k)
		mems = append(mems, storedMemory{fullKey: k, memType: memType, shortKey: shortKey, value: v})
	}
	sort.Slice(mems, func(i, j int) bool {
		if mems[i].memType != mems[j].memType {
			return memTypeRank(mems[i].memType) < memTypeRank(mems[j].memType)
		}
		return mems[i].shortKey < mems[j].shortKey
	})
	return mems, nil
}

// buildCompactPrompt renders the current memories and the output contract.
func buildCompactPrompt(mems []storedMemory) string {
	var b strings.Builder
	b.WriteString("You are compacting an AI agent's persistent memory store. ")
	b.WriteString("Each memory has a type, a short key, and a value.\n\n")
	b.WriteString("Goals, in priority order:\n")
	b.WriteString("1. Merge memories that overlap or restate the same fact into ONE clear memory.\n")
	b.WriteString("2. Drop memories that are stale, redundant, or fully superseded by another.\n")
	b.WriteString("3. Preserve every distinct fact — never lose information, never invent new facts.\n")
	b.WriteString("4. Keep each memory's type the same category it had (feedback, user, project, reference, general).\n")
	b.WriteString("5. Keep values concise but complete.\n\n")
	b.WriteString("Current memories (each value is quoted; \\n denotes a newline inside a value):\n\n")
	for _, m := range mems {
		// Identify each memory as type/key (keys can repeat across types) and
		// quote the value with %q so embedded newlines can't break the one
		// memory-per-line structure the model reads.
		fmt.Fprintf(&b, "- %s/%s: %q\n", m.memType, m.shortKey, m.value)
	}
	b.WriteString("\nReturn ONLY a JSON object (no prose, no markdown fences) of this exact shape:\n")
	b.WriteString(`{
  "memories": [
    {"type": "feedback|user|project|reference|general", "key": "kebab-case-key", "value": "merged text", "sources": ["type/key-1", "type/key-2"]}
  ],
  "dropped": [
    {"key": "type/key", "reason": "why it was removed"}
  ]
}
`)
	b.WriteString("\n\"memories\" is the COMPLETE desired final set — include every memory you want to keep, ")
	b.WriteString("merged or unchanged. Identify originals in \"sources\" and \"dropped\" by their full \"type/key\" ")
	b.WriteString("(as shown above), since the same key can appear under different types. \"sources\" lists the ")
	b.WriteString("originals each final memory consolidates (a single-source list is fine for unchanged memories). ")
	b.WriteString("\"dropped\" explains memories you removed entirely. If no compaction is warranted, return every memory unchanged.")
	return b.String()
}

// invokeClaudeCompact runs the claude CLI headless and returns its raw stdout.
// CLAUDECODE env vars are cleared so an agent running this from inside a Claude
// Code session does not trip the nested-session guard (same approach as seance).
func invokeClaudeCompact(ctx context.Context, prompt, model string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "claude",
		"--dangerously-skip-permissions",
		"--output-format", "json",
		"--max-turns", "1",
		"--model", model,
		"-p", prompt,
	)
	cmd.Env = clearClaudeCodeEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// parseCompactResponse unwraps the claude JSON envelope and extracts the
// embedded compaction JSON object from the result text.
func parseCompactResponse(raw []byte) (*memCompactResult, error) {
	var env struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	resultText := string(raw)
	if err := json.Unmarshal(raw, &env); err == nil {
		if env.IsError {
			return nil, fmt.Errorf("model reported an error (subtype %q): %s", env.Subtype, strings.TrimSpace(env.Result))
		}
		if env.Result != "" {
			resultText = env.Result
		}
	}

	obj := extractJSONSpan(resultText)
	if obj == "" {
		return nil, fmt.Errorf("no JSON object found in model output: %s", truncateStr(strings.TrimSpace(resultText), 200))
	}

	var result memCompactResult
	if err := json.Unmarshal([]byte(obj), &result); err != nil {
		return nil, fmt.Errorf("decoding compaction JSON: %w", err)
	}
	return &result, nil
}

// extractJSONSpan pulls the compaction JSON object out of the model's text.
// It first unwraps a fenced ```json … ``` (or bare ``` … ```) block if present
// — the most common way a model wraps structured output despite being asked
// not to — then falls back to the span from the first '{' to the last '}'.
func extractJSONSpan(s string) string {
	if fenced := unwrapCodeFence(s); fenced != "" {
		s = fenced
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// unwrapCodeFence returns the body of the first ```-fenced block in s (with an
// optional language tag like "json"), or "" if there is no closed fence.
func unwrapCodeFence(s string) string {
	open := strings.Index(s, "```")
	if open < 0 {
		return ""
	}
	rest := s[open+3:]
	// Skip an optional language tag up to the end of the line.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		firstLine := strings.TrimSpace(rest[:nl])
		if firstLine == "" || !strings.ContainsAny(firstLine, " \t{}") {
			rest = rest[nl+1:]
		}
	}
	if close := strings.Index(rest, "```"); close >= 0 {
		return rest[:close]
	}
	return ""
}

// buildCompactPlan turns the LLM's desired final set into a deterministic set
// of writes and deletes. Deletions are computed purely as set difference, so
// the model cannot delete a memory it still listed under "memories".
func buildCompactPlan(originals []storedMemory, result *memCompactResult) (*compactPlan, error) {
	if len(result.Memories) == 0 {
		return nil, fmt.Errorf("refusing to apply: model returned an empty memory set (this would erase all %d memories)", len(originals))
	}

	// Index originals by exact key and by (type, shortKey) so unchanged
	// memories — including legacy untyped ones — reuse their existing key
	// instead of being rewritten under a new memory.<type>.<key> slug.
	//
	// A (type, shortKey) pair can map to more than one kv key — e.g. a legacy
	// untyped "memory.foo" and a typed "memory.general.foo" both resolve to
	// general/foo. Pick deterministically rather than letting map-iteration
	// order decide which survives: prefer the canonical memory.<type>.<key>
	// form so the legacy duplicate is the one cleared.
	origByFullKey := make(map[string]storedMemory, len(originals))
	origByTypeKey := make(map[string]string, len(originals))
	for _, m := range originals {
		origByFullKey[m.fullKey] = m
		typeKey := m.memType + "/" + m.shortKey
		canonical := memoryKeyPrefix + m.memType + "." + m.shortKey
		if existing, ok := origByTypeKey[typeKey]; !ok || (m.fullKey == canonical && existing != canonical) {
			origByTypeKey[typeKey] = m.fullKey
		}
	}

	plan := &compactPlan{dropReasons: map[string]string{}}
	finalFullKeys := map[string]bool{}

	for i, cm := range result.Memories {
		memType := strings.ToLower(strings.TrimSpace(cm.Type))
		if memType == "" {
			memType = "general"
		}
		if _, ok := validMemoryTypes[memType]; !ok {
			return nil, fmt.Errorf("model returned memory %d with invalid type %q", i, cm.Type)
		}
		shortKey := sanitizeKey(cm.Key)
		if shortKey == "" {
			return nil, fmt.Errorf("model returned memory %d (type %s) with an empty key", i, memType)
		}
		if strings.TrimSpace(cm.Value) == "" {
			return nil, fmt.Errorf("model returned memory %q with an empty value", shortKey)
		}

		fullKey := memoryKeyPrefix + memType + "." + shortKey
		if existing, ok := origByTypeKey[memType+"/"+shortKey]; ok {
			fullKey = existing // preserve legacy/exact key
		}
		if finalFullKeys[fullKey] {
			return nil, fmt.Errorf("model returned duplicate memory key %s", fullKey)
		}
		finalFullKeys[fullKey] = true

		prev, existed := origByFullKey[fullKey]
		plan.sets = append(plan.sets, memSetOp{
			fullKey:  fullKey,
			memType:  memType,
			shortKey: shortKey,
			value:    cm.Value,
			sources:  cm.Sources,
			isNew:    !existed,
			changed:  existed && prev.value != cm.Value,
		})
	}

	for _, m := range originals {
		if !finalFullKeys[m.fullKey] {
			plan.clears = append(plan.clears, m)
		}
	}

	// Map the model's drop reasons onto the keys we will actually clear.
	for _, d := range result.Dropped {
		for _, m := range plan.clears {
			if d.Key == m.fullKey || d.Key == m.shortKey || d.Key == m.memType+"/"+m.shortKey {
				plan.dropReasons[m.fullKey] = d.Reason
			}
		}
	}

	return plan, nil
}

// renderCompactPlan prints a human-readable summary of the proposed changes.
func renderCompactPlan(originals []storedMemory, plan *compactPlan) {
	fmt.Printf("%s (%d → %d memories)\n\n",
		style.Bold.Render("Compaction plan"), len(originals), len(plan.sets))

	for _, s := range plan.sets {
		label := s.memType + "/" + s.shortKey
		switch {
		case s.isNew && len(s.sources) > 1:
			fmt.Printf("  %s %s\n", style.Success.Render("MERGE "), style.Bold.Render(label))
			for _, src := range s.sources {
				fmt.Printf("         %s %s\n", style.Dim.Render("←"), src)
			}
		case s.isNew:
			fmt.Printf("  %s %s\n", style.Success.Render("NEW   "), style.Bold.Render(label))
		case s.changed:
			fmt.Printf("  %s %s\n", style.Info.Render("UPDATE"), style.Bold.Render(label))
			if len(s.sources) > 1 {
				for _, src := range s.sources {
					fmt.Printf("         %s %s\n", style.Dim.Render("←"), src)
				}
			}
		default:
			fmt.Printf("  %s %s\n", style.Dim.Render("KEEP  "), style.Dim.Render(label))
		}
	}

	for _, m := range plan.clears {
		// Skip clears folded into a MERGE/UPDATE — they're already shown as a
		// "← src" line, so re-listing them as DROP would double-count them.
		if mergedAway(m, plan) {
			continue
		}
		reason := plan.dropReasons[m.fullKey]
		label := m.memType + "/" + m.shortKey
		if reason != "" {
			fmt.Printf("  %s %s  %s\n", style.Warning.Render("DROP  "), style.Bold.Render(label), style.Dim.Render("("+reason+")"))
		} else {
			fmt.Printf("  %s %s\n", style.Warning.Render("DROP  "), style.Bold.Render(label))
		}
	}
}

// mergedAway reports whether a cleared memory's key is listed as a source of
// some surviving merged/updated memory (so it's already shown as a "← src").
func mergedAway(m storedMemory, plan *compactPlan) bool {
	for _, s := range plan.sets {
		for _, src := range s.sources {
			if src == m.fullKey || src == m.shortKey || src == m.memType+"/"+m.shortKey {
				return true
			}
		}
	}
	return false
}

// applyCompactPlan writes the new/changed memories then clears the removed ones.
func applyCompactPlan(plan *compactPlan) error {
	wrote, cleared := 0, 0
	for _, s := range plan.sets {
		if !s.isNew && !s.changed {
			continue
		}
		if err := bdKvSet(s.fullKey, s.value); err != nil {
			return fmt.Errorf("writing %s: %w", s.fullKey, err)
		}
		wrote++
	}
	for _, m := range plan.clears {
		if err := bdKvClear(m.fullKey); err != nil {
			return fmt.Errorf("clearing %s: %w", m.fullKey, err)
		}
		cleared++
	}
	fmt.Printf("%s Compacted memories: %d written, %d removed.\n",
		style.Success.Render("✓"), wrote, cleared)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
