+++
name = "stuck-agent-dog"
description = "Context-aware stuck/crashed agent detection and restart for polecats and deacons"
version = 1

[gate]
type = "cooldown"
duration = "5m"

[tracking]
labels = ["plugin:stuck-agent-dog", "category:health"]
digest = true

[execution]
timeout = "5m"
notify_on_failure = true
severity = "high"
+++

# Stuck Agent Dog

Detects stuck or crashed polecats and deacons by inspecting tmux session context
before taking action. Unlike the daemon's blind kill-and-restart approach, this
plugin checks whether an agent is truly unresponsive before restarting.

**Design principle**: The daemon should NEVER kill workers. It detects and logs.
This plugin (running as a Dog agent with AI judgment) makes the restart decision
after inspecting tmux pane output for signs of life.

Reference: WAR-ROOM-SERIAL-KILLER.md, commit f3d47a96.

**DO NOT gate on system load.** The daemon evaluates system pressure before
dispatching this plugin. Do NOT check `uptime`, load average, or memory yourself.
Do NOT emit escalations for "system overloaded" — that is not a real gate here.
The only gate for this plugin is the 5-minute cooldown configured above.

## Scope — What You May and May NOT Touch

**IN SCOPE** (these are the ONLY sessions this plugin may inspect or act on):
- Polecat sessions (`<rig>-polecat-<name>`)
- Deacon session (`hq-deacon`)

**OUT OF SCOPE — NEVER touch these, under any circumstances:**
- **Crew sessions** (`<rig>-crew-<name>`, e.g. `gastown-crew-bear`). Crew lifecycle
  is managed by the overseer (human), not dogs. Crew members are persistent,
  long-lived, and user-managed. A crew session that looks idle is NOT stuck — it
  is waiting for its human. Killing a crew session destroys the overseer's active
  workspace and is a **critical incident**.
- **Mayor session** (`hq-mayor`)
- **Witness sessions** (`<rig>-witness`)
- **Refinery sessions** (`<rig>-refinery`)
- Any session not enumerated by `run.sh`

**This scope is absolute.** Do NOT extend it based on your own judgment.
`run.sh` enumerates exactly the sessions to check. If a session does not
appear in its `CRASHED` or `STUCK` lists, it does not exist for your purposes.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it: the script is the only place session enumeration, the scope limits
above, and the kill/restart actions are implemented.

```bash
cd <plugin dir> && bash run.sh
```

Run the command exactly as shown. Do not kill, restart or inspect any session
yourself from this description. Report the script's output.

## What run.sh checks

- **Polecats**: a polecat is a concern if it has hooked work and its tmux
  session or agent process is dead. Crashed sessions (dead, work on hook) are
  restarted; stuck sessions (session alive, agent dead) are cleaned up and
  restarted.
- **Deacon** (`hq-deacon`): heartbeat staleness is read from the JSON
  `timestamp` field in `deacon/heartbeat.json` (falling back to file mtime only
  if the timestamp is missing or malformed). A heartbeat past the
  >20m threshold counts as stuck only when there is also no recent session
  activity. A live Deacon with no `in_progress` work is not an actionable
  stuck-heartbeat event. Stale-heartbeat escalations use the
  stable fingerprint `stuck-agent-dog:deacon:stuck-heartbeat` (no age seconds).
- **Mass death**: if three or more agents are down in one cycle, the script
  escalates instead of restarting them, since that points to a systemic issue
  (Dolt outage, OOM) rather than individual crashes.
