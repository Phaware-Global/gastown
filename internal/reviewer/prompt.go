package reviewer

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/steveyegge/gastown/internal/config"
)

// The deterministic per-perspective review prompt is generated from two embedded
// templates, never hand-assembled by the reviewer agent:
//
//   - execution.md.tmpl is the SHARED execution contract — the single place the
//     "how to review" instructions live (diff/SHA targeting, round handling,
//     required codegraph tooling, evidence standard, the output JSON schema). It
//     is rendered once per pass with the pass's parameters.
//   - perspective.md.tmpl is the outer wrapper that combines the resolved
//     perspective "lens", the rendered shared contract (injected as the
//     SharedInstructions template variable), and an ExtraInstructions slot for
//     caller-supplied additions.
//
// Centralizing the execution contract here keeps the perspective markdown files
// focused purely on their lens and guarantees every pass — built-in or
// rig-authored — carries an identical, machine-oriented execution contract.
var (
	//go:embed prompt/execution.md.tmpl
	sharedExecutionContractTmpl string
	//go:embed prompt/perspective.md.tmpl
	perspectivePromptTmpl string
)

// PromptParams carries the deterministic inputs for generating one perspective
// review pass prompt. Everything an agent would otherwise have to infer (which
// SHA, which round, the finding cap, the output schema) is passed explicitly.
type PromptParams struct {
	// Perspective is the resolved perspective name (the lens).
	Perspective string
	// Lens is the resolved perspective prompt content (rig/town/builtin).
	Lens string
	// RigName scopes the prompt to a rig for the agent's orientation.
	RigName string
	// PR is the pull request number under review.
	PR int
	// SHA is the exact head commit the pass must review and anchor to.
	SHA string
	// Round is the review round (>=2 is a fix round; prior threads apply).
	Round int
	// PriorThreads is the deterministically-assembled prior-round thread context
	// (only meaningful when Round >= 2). The pass does not gather it itself.
	PriorThreads string
	// MaxFindings caps findings emitted by this pass (overflow summarized).
	MaxFindings int
	// ExtraInstructions is the injection slot for additional execution
	// instructions, appended after the shared contract. Usually empty.
	ExtraInstructions string
}

// BuildPerspectivePrompt renders the fully-resolved prompt for a single
// perspective review pass: the lens, the shared execution contract (injected via
// the SharedInstructions template variable), and any extra instructions. The
// result is the source of truth a review subagent executes from — no reviewer
// heuristics, no per-agent reinterpretation of the procedure.
func BuildPerspectivePrompt(p PromptParams) (string, error) {
	if strings.TrimSpace(p.Perspective) == "" {
		return "", fmt.Errorf("perspective name is required")
	}
	if strings.TrimSpace(p.Lens) == "" {
		return "", fmt.Errorf("perspective %q has empty lens content", p.Perspective)
	}
	if p.Round < 1 {
		p.Round = 1
	}
	if p.MaxFindings <= 0 {
		p.MaxFindings = config.DefaultMaxFindingsPerPerspective
	}
	p.PriorThreads = strings.TrimRight(p.PriorThreads, "\n")
	p.ExtraInstructions = strings.TrimSpace(p.ExtraInstructions)

	shared, err := renderPromptTemplate("reviewer-execution", sharedExecutionContractTmpl, p)
	if err != nil {
		return "", fmt.Errorf("rendering shared execution contract: %w", err)
	}

	outer := struct {
		PromptParams
		SharedInstructions string
	}{PromptParams: p, SharedInstructions: strings.TrimRight(shared, "\n")}

	out, err := renderPromptTemplate("reviewer-perspective", perspectivePromptTmpl, outer)
	if err != nil {
		return "", fmt.Errorf("rendering perspective prompt: %w", err)
	}
	return strings.TrimRight(out, "\n") + "\n", nil
}

// renderPromptTemplate parses and executes a text/template (not html/template:
// the output is a plain-text agent prompt, not HTML, and must not be escaped).
func renderPromptTemplate(name, tmpl string, data any) (string, error) {
	t, err := template.New(name).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}
