# Refinery Template Summary Contradicts Formula Overlay

## Problem

The refinery prime output contains two descriptions of the merge-push step that
can contradict each other:

1. **Template quick-reference** (`internal/templates/roles/refinery.md.tmpl` ~line 232):
   Hardcoded summary that always describes direct merge:
   ```
   **merge-push**: Merge to effective target and push immediately
   git checkout <merge-target>
   git merge --ff-only temp
   git push origin <merge-target>
   ```

2. **Formula step detail** (from `mol-refinery-patrol.formula.toml`, after overlay):
   The detailed Step 7 section which reflects the overlay content, e.g.
   "Create PR, run augment review loop, then escalate for human merge."

The template summary appears first in the prime output and is more concise. In
testing, both Haiku and Sonnet followed the summary's direct-merge instructions
and ignored the detailed overlay step that described the PR workflow.

## Evidence

- Prime output lines 232-239: direct merge quick-reference
- Prime output line 789+: overlay-replaced Step 7 with PR workflow
- Two consecutive refinery sessions (Haiku, then Sonnet) both performed
  `git merge --ff-only` + `git push origin main` despite Step 7 clearly
  stating `merge_strategy = pr` and describing the PR creation flow

## Workaround

Added a PreToolUse hook on `Bash(git push origin main*)` for the
jira_claude_channel refinery that hard-blocks direct pushes to main with a
message directing the agent to follow the PR workflow. This is belt-and-suspenders
enforcement — the agent hits a wall even when following the wrong instructions.

## Fork Fix

Make the template summary conditional on `merge_strategy`. The template already
has access to config variables via Go template rendering. The quick-reference
section should reflect the configured strategy:

```
{{- if eq .MergeStrategy "pr" }}
**merge-push**: Create GitHub PR, request review, escalate for human merge
  Do NOT push directly to main. Follow the detailed Step 7 instructions.
{{- else }}
**merge-push**: Merge to effective target and push immediately
  git checkout <merge-target>
  git merge --ff-only temp
  git push origin <merge-target>
{{- end }}
```

Alternatively, remove the hardcoded summary entirely and rely on the formula
step descriptions as the single source of truth.
