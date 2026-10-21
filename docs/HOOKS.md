# Gas Town Hooks Management

Centralized hook management for Gas Town workspaces.

## Overview

Gas Town manages context injection for all supported agents. The mechanism varies by agent:

| Agent | Hook mechanism | Managed file |
|-------|---------------|-------------|
| Claude Code, Gemini | `settings.json` lifecycle hooks | `<role>/.claude/settings.json` |
| OpenCode | JS plugin | `workDir/.opencode/plugins/gastown.js` |
| GitHub Copilot | JSON lifecycle hooks | `workDir/.github/hooks/gastown.json` |
| Codex | Managed hooks that run the role's Claude hooks | `/etc/codex/requirements.toml` (root-owned, machine-wide) |
| Others | Startup nudge fallback | *(no file — nudge only)* |

> **GitHub Copilot note**: Copilot CLI supports full executable lifecycle hooks
> (`sessionStart`, `userPromptSubmitted`, `preToolUse`, `sessionEnd`) via
> `.github/hooks/gastown.json`. This is the same lifecycle coverage as Claude Code,
> delivered in Copilot's JSON format rather than Claude's `settings.json` format.
> The `gt hooks` commands below apply to Claude Code (and Gemini) only.

> **Codex note**: Codex runs a project or user hook only after a human trusts
> its hash, so Gas Town installs Codex hooks as *managed* hooks in Codex's
> system requirements file instead. Each Codex event runs
> `gt hooks codex run <event>`, which runs the same hooks a Claude agent in
> that role gets: the base config plus role and rig overrides described below.
> Changing the base or an override therefore changes Codex agents too, with no
> reinstall. See [Codex managed hooks](#codex-managed-hooks).

Gas Town manages `.claude/settings.json` files in gastown-managed parent directories
and passes them to Claude Code via the `--settings` flag. This keeps customer repos
clean while providing role-specific hook configuration. The hooks system provides
a single source of truth with a base config and per-role/per-rig overrides.

## Architecture

```
~/.gt/hooks-base.json              ← Shared base config (all agents)
~/.gt/hooks-overrides/
  ├── crew.json                    ← Override for all crew workers
  ├── witness.json                 ← Override for all witnesses
  ├── gastown__crew.json           ← Override for gastown crew specifically
  └── ...
```

**Merge strategy:** `base → role → rig+role` (more specific wins)

For a target like `gastown/crew`:
1. Start with base config
2. Apply `crew` override (if exists)
3. Apply `gastown/crew` override (if exists)

## Generated targets

Each rig generates settings in shared parent directories (not per-worktree):

| Target | Path | Override Key |
|--------|------|--------------|
| Crew (shared) | `<rig>/crew/.claude/settings.json` | `<rig>/crew` |
| Witness | `<rig>/witness/.claude/settings.json` | `<rig>/witness` |
| Refinery | `<rig>/refinery/.claude/settings.json` | `<rig>/refinery` |
| Polecats (shared) | `<rig>/polecats/.claude/settings.json` | `<rig>/polecats` |

Town-level targets:
- `mayor/.claude/settings.json` (key: `mayor`)
- `deacon/.claude/settings.json` (key: `deacon`)

Settings are passed to Claude Code via `--settings <path>`, which loads them as
a separate priority tier that merges additively with project settings.

## Commands

### `gt hooks sync`

Regenerate all `.claude/settings.json` files from base + overrides.
Preserves non-hooks fields (editorMode, enabledPlugins, etc.).

```bash
gt hooks sync             # Write all settings files
gt hooks sync --dry-run   # Preview changes without writing
```

### `gt hooks diff`

Show what `sync` would change, without writing anything.

```bash
gt hooks diff             # Show differences
gt hooks diff --no-color  # Plain output
```

### `gt hooks base`

Edit the shared base config in `$EDITOR`.

```bash
gt hooks base             # Open in editor
gt hooks base --show      # Print current base config
```

### `gt hooks override <target>`

Edit overrides for a specific role or rig+role.

```bash
gt hooks override crew              # Edit crew override
gt hooks override gastown/witness   # Edit gastown witness override
gt hooks override crew --show       # Print current override
```

### `gt hooks list`

Show all managed settings.local.json locations and their sync status.

```bash
gt hooks list             # Show all targets
gt hooks list --json      # Machine-readable output
```

### `gt hooks scan`

Scan the workspace for existing hooks (reads current settings files).

```bash
gt hooks scan             # List all hooks
gt hooks scan --verbose   # Show hook commands
gt hooks scan --json      # JSON output
```

### `gt hooks init`

Bootstrap base config from existing settings.local.json files. Analyzes all
current settings, extracts common hooks as the base, and creates overrides
for per-target differences.

```bash
gt hooks init             # Bootstrap base and overrides
gt hooks init --dry-run   # Preview what would be created
```

Only works when no base config exists yet. Use `gt hooks base` to edit
an existing base config.

### `gt hooks registry` / `gt hooks install`

Browse and install hooks from the registry.

```bash
gt hooks registry                  # List available hooks
gt hooks install <hook-id>         # Install a hook to base config
```

## Current Registry Hooks

The registry (`~/gt/hooks/registry.toml`) defines 7 hooks, 5 enabled by default:

| Hook | Event | Enabled | Roles |
|---|---|---|---|
| pr-workflow-guard | PreToolUse | Yes | crew, polecat |
| session-prime | SessionStart | Yes | all |
| pre-compact-prime | PreCompact | Yes | all |
| mail-check | UserPromptSubmit | Yes | all |
| costs-record | Stop | Yes | crew, polecat, witness, refinery |
| clone-guard | PreToolUse | No | crew, polecat |
| dangerous-command-guard | PreToolUse | Yes | crew, polecat |

Additional hooks exist in settings.json files but are not yet in the registry:

- **bd init guard** (gastown/crew, beads/crew) - blocks `bd init*` inside `.beads/`
- **mol patrol guards** (gastown roles) - blocks persistent patrol molecules
- **tmux clear-history** (gastown root) - clears terminal history on session start
- **SessionStart .beads/ validation** (gastown/crew, beads/crew) - validates CWD

## Design Decision: Registry as Catalog vs Source of Truth

> **Decision: The registry is a catalog, not the source of truth.**
>
> The registry (`registry.toml`) lists available hooks. The base/overrides system
> (`~/.gt/hooks-base.json` + `~/.gt/hooks-overrides/`) defines what is active.
> `gt hooks install` copies from the registry into the base/overrides config.
>
> This separation provides:
> - Per-machine customization (PATH differences across machines)
> - Per-role overrides without polluting the shared registry
> - Clear distinction between "what hooks exist" and "what hooks are active where"
>
> The registry is the menu. The base/overrides are the order.

## Known Gaps

1. **Registry doesn't cover all active hooks** — Several hooks in settings.json
   files are not in `registry.toml` (bd-init-guard, mol-patrol-guard, tmux-clear,
   cwd-validation). These should be added so `gt hooks install` can manage them.

2. **No `gt tap` commands beyond pr-workflow** — The tap framework has only one
   guard implemented. `gt tap guard dangerous-command` is referenced in the
   registry but does not exist yet. Priority order: dangerous-command, bd-init,
   mol-patrol, then audit git-push.

3. **No `gt tap disable/enable` convenience commands** — Per-worktree
   enable/disable is possible via the override mechanism (`gt hooks override`
   with empty hooks list), but there is no convenience wrapper yet.

4. **Private hooks (settings.local.json)** — Claude Code supports
   `settings.local.json` for personal overrides. Gas Town doesn't manage
   these yet. Low priority since Gas Town is primarily agent-operated.

5. **Hook ordering** — No action needed currently. The merge chain
   (base -> override) produces deterministic order, and per-matcher merge
   ensures one entry per event type.

## Integration

### `gt rig add`

When a new rig is created, hooks are automatically synced for all the
new rig's targets (crew, witness, refinery, polecats).

### `gt doctor`

The `hooks-sync` check verifies all settings.local.json files match what
`gt hooks sync` would generate. Use `gt doctor --fix` to auto-fix
out-of-sync targets.

## Per-matcher merge semantics

When an override has the same matcher as a base entry, the override
**replaces** the base entry entirely. Different matchers are appended.
An override entry with an empty hooks list **removes** that matcher.

Example base:
```json
{
  "SessionStart": [
    { "matcher": "", "hooks": [{ "type": "command", "command": "gt prime" }] }
  ]
}
```

Override for witness:
```json
{
  "SessionStart": [
    { "matcher": "", "hooks": [{ "type": "command", "command": "gt prime --witness" }] }
  ]
}
```

Result: The witness gets `gt prime --witness` instead of `gt prime`
(same matcher = replace).

## Default base config

When no base config exists, the system uses sensible defaults:

- **SessionStart**: PATH setup + `gt prime --hook`
- **PreCompact**: PATH setup + `gt prime --hook`
- **UserPromptSubmit**: PATH setup + `gt mail check --inject`
- **Stop**: PATH setup + `gt costs record`
- **PreToolUse**: `pr-workflow` and `command-substitution` on every Bash call,
  `dangerous-command` on `rm -rf /` and force pushes, `push-main` on pushes,
  and `memory-write` on Write/Edit/NotebookEdit

The built-in overrides add `dangerous-command` on `sudo` and system package
installs for the autonomous roles (polecats, witness, refinery, deacon, boot,
reviewer, assistant). A Stop override for polecats repeats `gt costs record`
next to `gt tap polecat-stop-check`, because an override entry with the same
matcher replaces the base entry.

`gt hooks sync` writes this computed config over the hooks a settings
template installed, so every guard has to be here, not only in the template;
`TestComputeExpectedKeepsTemplateHooks` fails when a template hook would be
lost.

## Codex managed hooks

Codex agents get their hooks from `/etc/codex/requirements.toml`. Codex trusts
hooks in that file by policy, and an agent cannot edit a root-owned file, so it
cannot remove or untrust its own guards. Install or update it with:

```bash
gt hooks codex requirements | sudo tee /etc/codex/requirements.toml
gt hooks codex check
```

The file has one hook per event (SessionStart, UserPromptSubmit, PreToolUse,
PostToolUse, PreCompact, Stop), each calling `gt hooks codex run <event>` by
the gt binary's full path. It sets `allow_managed_hooks_only = true`, so Codex
runs no user, project, session or plugin hooks: an agent never stops at the
"Hooks need review" dialog (the beads plugin ships hooks of its own) and
cannot add hooks of its own. It also pins `[features] hooks = true`, so a
session cannot turn hooks off, and `memories = false`, so Codex's own memory
files cannot hold policy outside the formula, beads and mail. The file names
the gt binary by path, so reinstall it only when that path changes. Gas Town
refuses to start a Codex agent until `gt hooks codex check` passes.

Codex also stops at a "Trust this folder?" dialog the first time it opens a
project, and reads that trust only from the user's `~/.codex/config.toml`
(`$CODEX_HOME`), not from a `-c` override. Polecats run in a new worktree per
name, so before a Codex agent starts, Gas Town adds a trusted
`[projects."<path>"]` table for its work dir, the work dir's git toplevel and,
for a linked worktree, the main checkout. Existing tables, including one a
person marked untrusted, are left as they are.

The dispatcher translates between Codex and Claude hook payloads:

| Codex | Runs the Claude hooks for |
|-------|---------------------------|
| `Bash` (shell and `exec_command`) | `Bash`, with `Bash(<glob>)` matchers tested against the command and each subcommand split at `&&`, `\|\|`, `;`, `\|` and newlines |
| `apply_patch` | `Edit`, once per file the patch adds, updates, deletes or moves to, with `tool_input.file_path` resolved against the session cwd |
| `request_user_input` | `AskUserQuestion` |
| MCP tools (`mcp__server__tool`) | the same name |
| SessionStart `source`, PreCompact `trigger` | the matcher value, as in Claude |

A hook that exits 2 blocks the call and no later hook runs. Codex ignores an
exit-2 block with an empty stderr, so when a guard writes its reason to stdout
(the inline `echo … && exit 2` guards), the dispatcher passes that as the
reason. Stdout reaches Codex as context only for SessionStart and
UserPromptSubmit (`additionalContextLimit = 0` keeps `gt prime` output inline
rather than spilled to a file); Stop and PostToolUse pass it only when it is a
JSON object, which is all Codex accepts there. Codex ignores PreCompact
stdout, so context comes back after compaction from the SessionStart hook,
which Codex fires again with `source` set to `compact`.

The dispatcher runs for every Codex session on the machine and does nothing
outside a Gas Town agent context, so a person's own Codex sessions get no
Gas Town hooks.
