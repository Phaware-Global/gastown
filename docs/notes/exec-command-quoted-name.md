# Literal Quotes in --name Flag via exec.Command

## Problem

The mayor's agent config in `settings/config.json` wraps the `--name` value in
escaped quotes:

```json
"args": ["--name", "\"Gas Town Mayor\"", ...]
```

When Go's `exec.Command` passes arguments, each array element is already a
discrete argument — no shell interpretation occurs. The escaped quotes become
literal characters in the name, showing up as `"Gas Town Mayor"` (with quotes)
in the Claude Code web UI when remote control is enabled.

## Location

Town settings: `~/gt/settings/config.json`, key `agents.claude-opus-remote-mayor.args`

## Fix

Remove the inner escaped quotes:

```json
"args": ["--name", "Gas Town Mayor", ...]
```

This is a config-only fix (no code change), but the pattern may recur if other
towns copy the example. The templates or documentation that generated this config
should be checked for the same issue.
