# Remote Polecat Execution — Socket Provider: Exec-Stream Re-attach

> **Date:** 2026-11-27
> **Author:** crew
> **Status:** Proposal
> **Core:** [remote-polecat-execution.md](remote-polecat-execution.md) (§5, §9.4, §9.5)
> **Extends:** [remote-polecat-execution-socket.md](remote-polecat-execution-socket.md) (§4.3, §5, §6, §7) — read it first.
> **Operator guide:** [../socket-worker.md](../socket-worker.md)

This spec changes what a lost exec stream means. Today it kills the agent. The
proposal is that the agent keeps running in its pty on the worker, the way a
tmux session survives a lost ssh connection, and the launcher re-attaches to the
**same** process with bounded output replay. It also defines when a session that
nobody re-attaches to is reclaimed, so that worker slots do not leak.

File references are to `main` at the time of writing.

---

## 1. Problem

### 1.1 Incident shape

A worker with `-max-sessions 3` runs three polecats. The orchestrator side of
all three exec streams goes away within seconds (the launcher processes were
terminated; a network blip, a killed pane or an orchestrator reboot has the same
effect). The worker logs, per session, one of:

- `exec stream read failed; canceling the attached agent`
- `exec stream liveness probe failed; canceling the attached agent`
- `canceled agent not reaped and nothing is draining its terminal; closing the master`

All three agents are killed, one of them mid-work. Afterwards the worker's
`sessions.json` still lists the three sessions as `ready`, each holding a slot,
although no pane exists on the orchestrator.

### 1.2 Current behaviour

**The agent's lifetime is the connection's lifetime.** `handleAttach`
(`internal/workerclient/exec.go:140-213`) requires a session in state `ready`
(`:179`, `:197-201`), acks, and calls `streamExec`. `streamExec` derives its
context from the per-connection context (`exec.go:217`; the connection context
is created in `handle`, `service.go:296-297`) and builds the agent with
`exec.CommandContext` on it (`exec.go:594`, `:604`). Cancelling sends `SIGTERM`
to the agent's process group (`exec.go:622-627`) and `SIGKILL` after
`execWaitDelay`, 10s (`exec.go:27`, `:599`, `:628`).

**Three places cancel on a stream fault:**

1. Output pump: a frame write fails (`exec.go:343-350`). Writes are bounded by
   `frameWriteTimeout`, 60s (`service.go:244`, `:262-273`). The pump reads the
   pty master and writes to the socket in one loop, so a stalled launcher also
   stalls the agent's output for up to that long.
2. Inbound reader: any read error other than a clean EOF (`exec.go:411-419`). A
   clean EOF is a half-close and does not cancel (`exec.go:403-410`). The
   comment states the reason for cancelling: "rather than leave an orphaned
   agent pinning the session with no reattach path".
3. Liveness prober: every `execProbeInterval`, 30s, it writes a zero-length
   stdout frame; a failed write cancels (`exec.go:455-473`). A write proves
   little: on a half-open TCP connection (partition or peer host crash, no FIN
   or RST) a few bytes go into the kernel send buffer and the write succeeds.
   The listener sets no keepalive options of its own
   (`cmd/gt-worker-client/main.go`), so detection of such a peer is left to
   the platform's TCP timers and can take minutes.

After cancellation `waitReleasing` reaps the agent, and if the pump has exited
and the agent is still not reaped after `ptyWaitGrace` (15s) it closes the pty
master (`exec.go:784-828`). The exit frame is written to the same, usually
dead, connection (`exec.go:531-535`); the exit code is not kept anywhere else.

**pty and stdio.** With `tty` set the worker allocates a pty at the launcher's
geometry (`exec.go:268-295`, `pty.go:91-158`); the master is the agent's stdin
and its single output stream. Resize frames call `tty.resize`
(`exec.go:429-443`). Signal frames go to the agent's process group, or to
`docker kill` in container mode (`exec.go:720-735`).

**One exec per session.** `session.execCancel` is the registration
(`service.go:102`). A second attach while it is set is refused with exit 125,
as is an attach during teardown (`exec.go:225-238`). The registration is
cleared when the exec ends (`exec.go:240-247`, `:525-529`).

**What `ready` means.** A session becomes `ready` when bring-up has finished:
identity, relay, clone, checkpoint supervisor (`service.go:805-808`). It says
nothing about an agent. A session whose agent was cancelled stays `ready` and
stays in `s.sessions`, which is what `MaxSessions` counts
(`service.go:454-459`). It leaves only through `teardown`
(`service.go:927-1007`), a failed bring-up, or the supervisor's watchdog
(`max_runtime`, or the dead-man switch at 4× the checkpoint interval:
`internal/worker/supervisor.go:73-75`, `:120-130`), which moves it to the
orphan set (`service.go:1014-1044`). `sessions.json` is a snapshot of live
sessions plus orphans (`service.go:1110-1135`).

**The launcher never retries.** `gt worker-attach` dials once
(`internal/workerattach/attach.go:84-91`), sends one attach (`:108-120`) and
exits 1 on any stream error or EOF before an exit frame (`:236-245`). It
forwards `SIGINT`, `SIGTERM`, `SIGHUP` and `SIGQUIT` to the agent as signal
frames, and for the three terminating ones exits 143 after 10s if the agent has
not ended the stream (`:187-210`, `:267`). `KillSessionWithProcesses` sends the
pane process `SIGTERM` and `SIGKILL` 2s later
(`internal/tmux/tmux.go:714-716`), so an intentional session kill reaches the
agent twice: as one forwarded `SIGTERM`, which an agent may ignore, and as a
dropped stream. The drop is what enforces the kill: it cancels the exec, which
escalates to `SIGKILL`. `SessionManager.Stop` and `gt polecat nuke`
(`internal/cmd/polecat.go:1893`) depend on that.

**Orchestrator recovery.** `SessionManager.Start` provisions before it creates
the pane (`internal/polecat/session_manager.go:535-567`). `provisionOnce`
returns early, without `session_open`, when the handshake lists the session in
any state but `orphaned` (`internal/socket/backend.go:209-215`). So there are
two recovery paths today:

- Session still `ready` on the worker: the new pane's launcher attaches and a
  **new** agent starts in the existing worktree.
- Session orphaned or gone (worker restarted, watchdog fired, torn down): a full
  bring-up runs. It removes the worktree and clones again (`service.go:658-663`)
  and resets the branch to the polecat's pushed work ref if one exists
  (`service.go:1237-1258`).

The daemon does not restart polecats itself. `checkPolecatHealth` reports a
dead tmux session with hooked work to the witness (`internal/daemon/daemon.go:3176-3272`,
`:3403-3418`); the stuck-agent-dog plugin confirms and asks the witness to
restart, and escalates when three or more agents are down in one cycle
(`plugins/stuck-agent-dog/run.sh:387-411`). The daemon has no respawn limit of
its own for polecats (`RestartTracker` gates the Deacon, `daemon.go:1685`,
`:1886`); the limit is per bead and applies when a polecat is spawned for it
again: after the configured number of respawns (3 by default) the spawn is
refused until `gt sling respawn-reset` (`internal/cmd/polecat_spawn.go:152-157`,
`internal/witness/handlers.go:2949-2998`). Every drop that ends in a restart
spends one of those attempts.

**Why the slots leak.** The only caller of `Backend.Teardown` outside failed
starts is `killIdlePolecat` (`daemon.go:3604-3630`), and `reapIdlePolecat`
returns immediately when the tmux session is not alive (`daemon.go:3515-3518`).
`SessionManager.Stop` kills the pane and does not tear down
(`session_manager.go:765-790`). Once the pane is gone, nothing on the
orchestrator releases the worker session. Orphans are not reaped either:
`"orphaned"` is read in one place (`backend.go:212`), and the orchestrator-side
capacity gate counts orphans (`backend.go:217-221`; `summaries()` includes
them, `service.go:1069-1094`).

**Checkpoints.** The supervisor commits tracked changes to
`refs/checkpoints/polecat/<name>` and force-pushes it each interval and on
shutdown (`internal/worker/checkpoint.go:107-162`, `supervisor.go:162-190`);
the proxy permits that ref (`internal/proxy/git.go:439-441`). No code restores
a worktree from the checkpoint ref: bring-up resumes from the
work ref only. Core §9.4's "reset its worktree to the checkpoint ref" is not
implemented for this provider.

**Worker restart.** `New` calls `loadPersisted` (`service.go:187`), which reads
`sessions.json` and marks every entry orphaned (`service.go:1139-1153`). On
`SIGTERM`, `Serve` runs `shutdownAll`
(`service.go:203-205`, `:1048-1066`), which stops each supervisor; attached
agents end because their connection contexts descend from the cancelled serve
context. Socket spec §7 says sessions are re-adopted and container agents keep
running across a restart; the code and `docs/socket-worker.md` ("Restarts and
orphans") say they are not.

### 1.3 What the existing specs anticipate

Core §9.4 and socket §5/§7 define "reattach" as `Provision` finding the
*session* live. That part exists (`backend.go:209-215`). Nothing describes
re-attaching to a running *agent*; the comment at `exec.go:408-410` names the
missing piece. The `§9.4` reference in `reapIdlePolecat` (`daemon.go:3524-3528`)
concerns a durable runtime anchor for `max_runtime`, which this proposal does
not change.

## 2. Goals and non-goals

### Goals

- A dropped exec stream does not signal, stall or otherwise disturb the agent.
- The launcher re-attaches to the same agent process without the tmux pane
  dying, with replay of output produced meanwhile, bounded by a ring buffer.
- An agent that exits while detached still has its exit code delivered, once.
- Re-attach is authenticated exactly as the first attach is.
- A session nobody returns to is reclaimed after a bounded time, with a final
  checkpoint, and its slot is freed on both sides.
- Mixed versions keep working: an old launcher against a new worker and a new
  launcher against an old worker both behave as today.

### Non-goals

- **Surviving a worker restart.** Agents are children of `gt-worker-client` and
  a native agent's terminal is a pty whose master only that process holds; when
  it exits the master closes and the agent is hung up. See §5.1.
- Lossless scrollback. Replay is bounded; a gap is reported, not hidden.
- Replaying keystrokes. Input lost with a connection stays lost.
- Changing the checkpoint protocol, or implementing checkpoint restore.
- Multiple simultaneous viewers of one agent.

## 3. Design

### 3.1 Worker: the session owns the exec

Split `streamExec` into two objects.

An **exec** belongs to the session and holds the command, the pty (or pipes),
an output ring, the last applied window size and, once the agent exits, its
exit code. Its context descends from a per-session context that `teardown`,
`shutdown`, the watchdog and `shutdownAll` cancel; it no longer descends from
any connection. One goroutine per output stream reads the master (or pipe) and
appends to the ring. That append never blocks, so the agent never blocks on
output, whether a launcher is attached, slow or absent. This also removes
today's coupling in which a stalled launcher stalls the agent for up to
`frameWriteTimeout`.

A **stream** belongs to a connection. It copies ring contents to frames from a
cursor, forwards stdin, resize and signal frames to the exec, and runs the
liveness check (§3.2). Any stream fault (write error, read error, liveness
timeout) ends the stream only, and the exec moves to `detached`.

Exec states: `attached`, `detached`, `exited` (exit code held, not yet
delivered). `session.execCancel` becomes a pointer to the exec. The session's
`ready` state is unchanged. The session records one timestamp, `idle_since`:
set when the session becomes `ready` and whenever its last stream ends (the
exec detaches, or exits with no stream attached), cleared while a stream is
attached. It is not reset when a detached exec is later dropped, so it always
means "no launcher since". `SessionSummary` gains optional fields `exec`
(`attached` | `detached` | `exited`), `exec_started_at` and `idle_since`, so
`discover` and `sessions.json` show what the incident could not. Every timer
in §4, on the worker and in the daemon, reads `idle_since`.

**Stopping the exec is the supervisor's `StopWork`.** Every session stop ends
in the supervisor's shutdown, which calls `StopWork` and then takes the final
checkpoint (`supervisor.go:162-190`). Native mode wires no `StopWork` today
(`service.go:779-781`), and `orphanSession` runs only after `Run` has returned
(`service.go:790-803`), which is after that flush. So the hook is wired in both
modes and does three things in order: fence the session against new attaches
(a new flag tested beside `tearingDown` at `exec.go:227-230`; not that flag
itself, which `orphanSession` reads as "teardown owns the cleanup",
`service.go:1026-1029`), cancel the exec, and wait until
the agent is reaped (bounded by `execWaitDelay` plus `ptyWaitGrace`); container
mode then runs the existing `workEnv.StopWork`. Dead-man, `max_runtime`, the
idle stop (§4), `shutdown`, `teardown` and `shutdownAll` all reach the agent
this way, so each final checkpoint captures a tree nobody is writing, and the
flush may skip its debounce because the stop is proven
(`supervisor.go:175-184`). `orphanSession` itself does not touch the exec.

The ring holds the most recent 1 MiB of output per session by default
(`-exec-buffer`), as `(stream, bytes)` chunks so the pipe mode keeps stdout and
stderr apart. A 64-bit counter of total bytes produced gives every byte an
offset. The ring is memory only and is never written to disk: it is terminal
output and can contain whatever the agent printed.

While detached, nothing is written to the agent's stdin and it is not closed
(closing a pty master hangs up the agent, `exec.go:391-399`). In pipe mode a
half-close before the drop has already closed stdin; a drop without one leaves
it open.

`waitReleasing`'s release valve keeps its condition (cancelled, and nothing is
reading the master). With a permanent reader it now fires only when the master
read itself has failed.

### 3.2 Protocol

`ProtoVersion` stays 1. Both sides require equality (`service.go:339-343`,
`internal/socket/conn.go:72-75`), and a busy worker defers its own upgrade
(`push.go:324-346`), so a bump would break exactly the mixed fleet this has to
tolerate. The feature is negotiated the way `agent_files` was: a capability.

- `hello_ack.capabilities.exec_resume: true` from a worker that implements this
  and has a non-zero detach timeout. `DialExecStream` currently discards the
  ack (`internal/socket/execstream.go:53-57`); it returns the capabilities.
- `attach_ack` gains `exec_id` (128-bit random, minted by the worker when the
  agent starts), `resumed` (bool), `offset` (the offset of the first byte this
  stream will send) and `truncated` (bytes between the requested offset and
  `offset` that the ring no longer holds; never negative).
- `attach` gains `resume` (an `exec_id`), `offset` (bytes the launcher has
  already written to its terminal) and `echo` (the launcher answers pings).
- Two frame types: `ping` (6, worker → launcher) and `pong` (7, launcher →
  worker), both with empty payloads. The worker sends `ping` only on a stream
  whose attach set `echo`.

Both frame readers already ignore unknown types (`attach.go:257-259`,
`exec.go:444-446`) and JSON decoding ignores unknown fields, so old peers are
unaffected.

**Liveness is round-trip evidence.** A successful write is not evidence
(§1.2). On an `echo` stream the worker sends `ping` every 10s and the launcher
answers `pong` at once. Each side records the arrival time of the last inbound
frame of any type and declares the stream dead after 30s without one; a read
or write error ends it sooner. The launcher applies the same rule to frames
from the worker, which is how it notices a silent partition and starts
redialling instead of sitting on a dead socket. Detection is therefore
bounded at about 30s on both sides, independent of TCP timers. After a clean
inbound EOF (a half-close, which stops pongs by design) and on streams without
`echo` (old launchers) the worker keeps today's write probe and its weaker
guarantee.

How the worker treats an attach to a session:

| Attach | Exec state | Result |
|---|---|---|
| no `resume` | none | start the agent (today) |
| no `resume` | `attached` | `echo` stream: send a `ping` now; an inbound frame within 5s means the stream is alive, refuse with 125 (today); none means it is dead, end it and continue as `detached`. Stream without `echo`: write probe, refuse with 125 unless the write fails (today) |
| no `resume` | `detached` | **replace**: cancel the old agent with the normal grace, then start the new one |
| no `resume` | `exited` | log and drop the undelivered code, start the agent |
| `resume` matches | `attached` | supersede: close the old connection, continue on this one |
| `resume` matches | `detached` | resume from `offset` |
| `resume` matches | `exited` | send remaining output, then the exit frame |
| `resume` unknown | any | `error` code `no_exec`; never start an agent |

A `resume` attach ignores `argv` and `env`; nothing is executed. "Replace" is
what an old launcher gets, and its end state equals today's (old agent ended,
new one in the same worktree). Supersession is safe because only the launcher
that received the `exec_id` can present it, and it does not depend on the
worker having noticed that the old socket is dead.

An `offset` greater than the exec's produced-byte counter cannot come from a
correct launcher. The worker clamps it to the counter, reports `truncated: 0`,
logs a warning and forces a repaint (below); it does not fail the resume,
because the agent is healthy and only the launcher's count is wrong.

**Old worker, new launcher.** An old worker would decode a `resume` attach as a
fresh one and start a second agent from the original argv. The launcher
therefore retries only when the handshake advertised `exec_resume`; otherwise
it exits on a stream fault as it does now.

**Replay and geometry.** On resume the worker sends ring contents from
`offset`, then live output. With no gap the launcher's terminal has received
every byte in order, so the screen is correct without a redraw. The `attach`
carries the pane's current `cols`/`rows`; the worker applies them when they
differ from the last applied size, which also makes a TUI repaint. When
`truncated` is non-zero the launcher prints one line to stderr
(`gt worker-attach: N bytes of output were lost while detached`) and the worker
forces a repaint by setting the size to `rows-1` and back, since setting an
unchanged size raises no `SIGWINCH`. Whether that is enough for the agent TUIs
in use needs testing (§8 Q4).

**Stdin.** While disconnected the launcher keeps reading its terminal into a
64 KiB buffer and sends it after resume, so a nudge typed into the pane during
a blip is delayed, not dropped; beyond the bound it discards and says so.
Frames already written to a connection that then died are not resent:
at-most-once, because a duplicated prompt is worse than a lost keystroke.

**Exit code.** If the agent exits while detached the exec becomes `exited` and
keeps the code and the ring. It is released when a stream has written the exit
frame and then read the launcher's EOF, or at the detach timeout. A launcher
that lost the connection after the write but before reading the frame resumes
and receives it again. A launcher process exits on the first exit frame it
reads, so the pane sees the code once. Exit codes remain diagnostic (socket §5).

### 3.3 Who re-attaches

**Recommendation: the launcher.** `gt worker-attach` wraps dial → attach →
pump in a loop. On a stream fault (an error, or 30s without an inbound frame,
§3.2), if the worker advertised `exec_resume` and
an `exec_id` is known, it keeps the terminal in raw mode, prints a one-line
status, and redials with backoff (1s doubling to 30s) for up to
`--reattach-window` (default 10 min). It exits when it reads an exit frame,
when the worker answers `no_exec` or `no_session` (exit 75, with a message), or
when the window expires (exit 75).

The alternative is to let the pane die and have the daemon recreate it. That
route is slow and indirect today (daemon mail → plugin → witness → `Start`),
and `Start` is built to launch a fresh agent: it sends a new beacon in argv and
may type startup nudges into the pane (`session_manager.go:599-696`), which
must not be typed into an agent that is mid-task. It also cannot help with the
most common fault, a network blip, without tearing down a healthy pane.

**Interplay with the daemon.** While the launcher retries, the tmux session
exists and its pane process is the launcher, which is what `GT_PROCESS_NAMES`
tracks for remote polecats (`session_manager.go:563-566`). `checkPolecatHealth`
sees a live session and does nothing (`daemon.go:3188-3191`); no crash mail, no
restart request, no mass-death event. The two mechanisms cannot fight because
the daemon acts only when the pane is gone, and the launcher gives up by
making the pane go.

When the launcher is the thing that died (killed process, killed pane,
orchestrator reboot), the existing crash path runs and `Start` creates a new
pane. Its launcher has no `exec_id`, so the attach is a **replace** (§3.2): the
detached agent is ended gracefully and a new one starts in the same worktree.
That is today's outcome, minus the agent having been killed at the moment of
the drop. Letting a new pane **adopt** the running agent is possible (an
`adopt` flag on attach, `Start` skipping its beacon and nudges when `discover`
reports a detached exec) but touches the start sequence and is deferred to an
optional phase (§7, §8 Q1).

**Launcher signals.** `SIGHUP` is what a launcher receives when its pane or the
tmux server goes away. With `exec_resume` it no longer forwards `SIGHUP`; it
closes the stream and exits, and the exec detaches. `SIGINT` is unchanged.

`SIGTERM` and `SIGQUIT` are still forwarded, but forwarding alone would be
weaker than today. The kill that `Stop` and `gt polecat nuke` rely on is
enforced by the dropped stream (§1.2), and under this design a drop only
detaches: an agent that ignores `SIGTERM` would keep running, with its
identity and relay live, until the detach timeout. So a forwarded `SIGTERM` or
`SIGQUIT` also **arms a kill on the worker**: the exec is marked `terminating`
and, unless the agent has exited, its process group gets `SIGKILL` after
`execWaitDelay` (in container mode the container's processes are terminated,
§4). Nothing disarms it: not a detach, not a resume. The bound is 10s from the
signal frame, tighter than today's (launcher grace, then the drop, then
`execWaitDelay`).

The armed kill needs the signal frame to arrive. If the launcher is killed
before it writes the frame, or the path is already partitioned, the agent
detaches and lives until the detach timeout. Paths that destroy a polecat
therefore must not rely on signals at all: `gt polecat nuke` and polecat
removal call `Backend.Teardown` for a remote rig (phase 3; today the worker
session, its identity and its relay outlive a nuke). `Stop`, which keeps the
session for a restart, stays signal-based until a session-scoped stop message
exists (phase 4).

### 3.4 Security

Re-attach arrives on the same listener as every other message and is
authenticated identically; there is no separate path to harden.

- TCP: TLS 1.3, client certificate required, chained to the client CA the
  worker pinned at enrollment, with the CN pinned to the orchestrator
  (`internal/workerclient/tlsconfig.go:40-53`). The launcher uses the
  orchestrator's enrollment material and pins the worker's enrolled name
  (`execstream.go:42-48`, `conn.go:135-169`), after the revocation check
  (`conn.go:108-130`).
- Unix socket: file permissions plus the pre-shared token, required as the
  first message (`service.go:323-331`).

A polecat identity cannot reach this listener: session certificates come from
the proxy CA, which is not the CA the worker trusts for clients (socket §10).

**What the handshake binds a session to today: nothing.** `attach`, `shutdown`
and `teardown` look a session up by id alone (`exec.go:161-162`,
`service.go:821-823`, `:878-879`). `hello.orchestrator_id` is the sender's
hostname (`internal/socket/config.go:142-148`) and the worker does not read
it. Any peer that passes the orchestrator check can already attach to, or tear
down, any session on that worker. Re-attach does not widen this, and the
`exec_id` adds a little: a peer that does not hold it cannot take over a live
stream, only be refused (attached) or replace the agent (detached), both of
which it can do today. `exec_id` is a correlation handle inside one trust
domain, not a credential, and travels only on the authenticated connection.

Binding sessions to the TLS key of the connection that opened them would
separate two orchestrators that share a worker. It is not proposed here (§8 Q5).

## 4. Reclamation

Two timers on the worker and one in the launcher, all flags with defaults. The
worker's both count from `idle_since` (§3.1):

- **Detach timeout** (`-detach-timeout`, default 15 min; 0 restores
  cancel-on-drop and withdraws the capability). A detached exec is cancelled
  when `idle_since` is this old: `SIGTERM` to the group, `SIGKILL` after
  `execWaitDelay`. In container mode cancelling ends only the `docker exec`
  client (`exec.go:595-598`), so the worker also terminates the container's
  processes the way the failed-pty path does (`exec.go:279-281`). An `exited`
  exec is dropped at the same age. The session stays `ready`.
- **Idle-session timeout** (`-idle-session-timeout`, default 30 min). A `ready`
  session whose `idle_since` is this old is stopped through the supervisor
  with a new stop reason. `StopWork` ends any exec first (§3.1), the final
  checkpoint flush runs, and the session follows the existing orphan path:
  relay closed, identity removed, worktree kept, slot freed
  (`service.go:1014-1044`). This is the rule that would have released the
  three sessions in §1.1.
- **Launcher window** (§3.3, default 10 min), shorter than the detach timeout
  so that a launcher giving up leaves time for a replace or adopt to find the
  agent alive.

`idle_since` starts when the worker *notices* the drop: at once for a closed
connection, within about 30s for an `echo` stream, and possibly much later for
an old launcher behind a half-open connection (§3.2). No ordering between
these timers and the watchdog is relied on. The dead-man switch fires 15 to 20
minutes after control-plane contact is lost at the default checkpoint interval
(`supervisor.go:120-130`), which can be before or after the detach timeout;
either way the agent is stopped before the final checkpoint, because that is
`StopWork`'s job and not the timer's.

**Session stops.** Every stop of a session goes through `StopWork` (§3.1);
the detach timeout, a replace and the armed kill (§3.3) end the exec directly
and leave the session running.
`handleShutdown` and `teardownSession` cancel the exec themselves today
(`service.go:856-861`, `:937-939`) and keep their fence; `shutdownAll` and the
watchdog stops gain the same behaviour through the hook instead of relying on
the connection.

**Slot accounting.** A session with a detached exec is a session: it counts
against `MaxSessions` on the worker (`service.go:454`) and in the orchestrator's
gate, which is correct, because its agent is using the machine. Orphans do not
count on the worker but do count in the orchestrator's gate
(`backend.go:217-221`); that gate should count only non-orphaned sessions, and
`Provision` should tear down an orphan carrying its own session id before
opening.

**Orchestrator.** `Teardown` is unchanged and remains authoritative: it ends
the exec, flushes and removes the session whatever the exec state. The idle
reaper is unchanged for live panes. A remote polecat's heartbeat is touched by
its proxied `gt` calls (`internal/proxy/exec.go:283-287`), independent of the
exec stream, so a detached agent that is working does not look idle.

New in phase 3: a daemon sweep that calls `Discover` per remote rig and tears
down a session when its polecat has no tmux session and it is either orphaned
or has an `idle_since` older than the idle-session timeout. It never uses the
session's age. That is the reaper sweep core §5 describes; the worker-side
timer is the backstop when the orchestrator is the thing that is gone.

Both the sweep and the worker's idle timer can race `Start`, which provisions
first and creates the pane afterwards (`session_manager.go:535-572`); on the
early-return path `Provision` tells the worker nothing
(`backend.go:209-215`). Two additions close the gap:

- **`touch`.** On the early-return path `Provision` sends a `touch` for the
  session (a new control message, sent only to `exec_resume` workers). The
  worker resets `idle_since` to now, so the pane has a full idle timeout to
  attach. If the worker answers `no_session`, the session was reclaimed between
  the handshake and the touch, and `Provision` continues into `session_open`
  instead of returning an endpoint that no longer exists.
- **Conditional teardown.** The sweep's `teardown` carries the `idle_since` it
  observed. The worker refuses with `busy` when a stream is attached or
  `idle_since` has changed. The sweep also re-checks for the tmux session
  immediately before sending.

`Teardown` from every other caller stays unconditional.

**Staged worker binary.** `ApplyPendingWorkerClient` applies only when
`len(s.sessions) == 0` (`push.go:324-346`). That stays: any session, attached
or detached, defers the restart, because the restart would kill its agent
(§5.1). Today a leaked `ready` session defers an upgrade indefinitely; with the
idle-session timeout the wait is bounded, and the orphan transition should call
`applyPendingIfIdle` as teardown does (`service.go:1006`).

## 5. Failure modes

| # | Fault | Worker | Orchestrator | Outcome |
|---|---|---|---|---|
| 1 | Launcher killed (`SIGKILL`), or pane killed (`SIGHUP`) | Connection closes; exec `detached`, agent keeps running | Pane gone; crash path asks for a restart if work is hooked | New pane **replaces** the agent (or adopts, phase 4). No restart within the detach timeout: agent stopped; later the session is orphaned |
| 1a | Intentional kill (`Stop`, nuke): launcher gets `SIGTERM`, then `SIGKILL` 2s later | Signal frame arms the kill: agent gone within `execWaitDelay` even if it ignores `SIGTERM` and the stream drops. Frame never arrived: as 1 | Nuke also calls `Teardown` (phase 3) | Agent ended within 10s of the frame; without the frame, by `Teardown` or the detach timeout |
| 2 | Network partition, no FIN or RST | `echo` stream: no inbound frame for 30s, `detached`. Writes may keep succeeding and prove nothing | No inbound frame for 30s; closes the socket and redials with backoff | Resume with replay when the path returns inside the window |
| 3 | Launcher reconnects before the worker has declared the old stream dead | Old stream still `attached` | Launcher presents `exec_id` | Supersede: worker closes the stale connection and continues on the new one |
| 4 | Partition longer than the launcher window | `detached`; the detach timeout or the dead-man switch, whichever is first, stops the agent; the dead-man stop takes a final local checkpoint after `StopWork` and orphans the session | Launcher exits 75; pane dies; crash path | Restart goes through bring-up; pushed work is resumed from the work ref |
| 5 | Orchestrator reboot | As 1 | Launchers get `SIGHUP` and/or `SIGTERM` in no fixed order; a forwarded `SIGTERM` arms the kill and ends the agent | Detached agents are replaced on restart, or reclaimed by timers. Best effort (§8 Q2) |
| 6 | Worker restart or crash | All agents die with the process; sessions load as orphaned | Launcher's resume gets `no_exec`/`no_session`, or the dial fails for the whole window; exits 75 | Unchanged from today: bring-up again. Out of scope (§5.1) |
| 7 | Agent exits while detached | Exec `exited`; code and ring kept until delivered or the detach timeout | Launcher resumes, receives remaining output and the exit frame, exits with the code | Pane sees the real exit code once |
| 8 | Two attaches race | Decided under the session lock: same `exec_id` supersedes; no `resume` against a stream that answers the ping is refused 125, against one that does not it replaces; two fresh attaches, one starts the agent and the other is refused | The refused launcher exits 125 | One agent per session, as today |
| 9 | Ring overflow while detached | Oldest bytes dropped; counter keeps advancing | Ack reports `truncated`; launcher prints the notice; forced repaint | Screen recovers; lost scrollback is reported |
| 10 | Launcher attached but too slow | Its cursor falls out of the ring; worker closes that stream | Launcher resumes | Becomes case 9; the agent never blocked |
| 11 | Old launcher (no `echo`) behind a half-open connection | Write probe keeps succeeding; exec stays `attached` until TCP gives up; a fresh attach meanwhile is refused 125, as today | Old launcher exits on its own read error or never notices | No worse than today for old launchers; the watchdog still stops the agent before its final checkpoint. Upgrading the launcher removes the case |

### 5.1 Worker restart

Not achievable without a process that outlives `gt-worker-client` and holds the
pty master. A native agent is the session leader of its pty
(`exec.go:607-616`); when the worker exits, the master closes and the kernel
hangs the agent up. A container agent's `docker exec` client is likewise a
child holding the outer pty. Surviving a restart needs a per-session holder
process, or running the agent under a terminal multiplexer (§6), plus the
re-adoption socket §7 describes and the code defers. This proposal does not do
that. It does keep the cost visible: a staged binary is not applied while any
session exists, and `docs/socket-worker.md` keeps its advice to restart a
worker only when it is idle.

## 6. Alternatives considered

- **Run the agent under tmux or screen on the worker.** It would give detach,
  scrollback and survival across a worker restart. It adds a dependency the
  worker contract does not have (`docs/socket-worker.md`, "What has to be on
  the worker"), a second terminal emulator between the agent and the pane
  (nested multiplexers alter key handling and repaint behaviour), and it still
  needs every protocol change in §3.2, because the launcher has to say which
  thing it is re-attaching to. Worth revisiting only if restart survival
  becomes a goal.
- **Keep cancel-on-drop, checkpoint harder.** A final flush on drop shortens
  the window of lost tracked changes but not the real cost: the agent's
  conversation, its in-flight tool calls and untracked files are gone, and no
  code restores a worktree from the checkpoint ref today (§1.2).
- **Application keepalive only.** Longer probe intervals or more tolerant
  timeouts would ride out a short stall. They do nothing for a closed
  connection or a dead launcher, and they lengthen the time a truly dead peer
  pins a session.
- **Daemon recreates the pane on every drop.** Rejected as the primary path in
  §3.3; kept as optional adoption.
- **Bump `ProtoVersion`.** Rejected in §3.2.

## 7. Implementation phases

| Phase | Change | Rough size | Tests |
|---|---|---|---|
| 1 | Worker: session-owned exec, ring buffer, `detached`/`exited` states, replace-on-fresh-attach, `idle_since`, detach timeout (default 0 in this phase, so behaviour is unchanged until phase 2), `StopWork` wired in both modes to fence, cancel and reap the exec, container kill on cancel, summary fields | ~600 lines + tests in `internal/workerclient` | Unit: ring wrap and offsets; agent survives a closed connection and keeps producing; output never blocks with no reader; detach timeout cancels; replace ends the old agent before starting the new; teardown, shutdown, `shutdownAll`, dead-man and `max_runtime` each reap a detached native agent before the final checkpoint is taken, and refuse an attach during the flush; existing exec and pty tests unchanged with timeout 0 |
| 2 | Protocol and launcher: capability, `exec_id`/`offset`/`truncated`, `ping`/`pong` liveness on both sides, resume and supersede, exit-code retention, launcher retry loop, stdin buffer, `SIGHUP` detaches, armed kill on forwarded `SIGTERM`/`SIGQUIT` | ~550 lines in `sockproto`, `socket`, `workerattach`, `workerclient` | Unit: every row of the §3.2 table; byte-exact replay across a cut at arbitrary offsets; exit while detached delivered once; `no_exec` never starts an agent; an offset beyond the counter is clamped; a peer that stops answering pings while its writes still succeed (a fake connection that accepts and discards) is declared dead in 30s, and a fresh attach then replaces instead of getting 125; a `SIGTERM`-ignoring agent is killed `execWaitDelay` after the signal frame whether the stream then stays, drops or resumes. Launcher: redials after 30s of silence; retries only with the capability, backoff, window expiry exits 75, terminal restored on every exit. Compat: new launcher against a capability-less fake worker |
| 3 | Reclamation: idle-session timeout with flush, orphan triggers `applyPendingIfIdle`, orchestrator gate ignores orphans and reaps its own, `touch` from `Provision`, daemon sweep with conditional teardown, `Teardown` from nuke and polecat removal, detach timeout default 15 min | ~500 lines in `workerclient`, `sockproto`, `socket`, `daemon`, `cmd` | Unit: idle session is flushed then orphaned and the slot is free; staged binary applies after reclamation; gate arithmetic with orphans; sweep tears down only sessions with no tmux session whose `idle_since` is past the timeout, never by session age; a `touch` or an attach between the sweep's read and its teardown yields `busy`; `Provision` falls through to `session_open` on `no_session`; nuke of a remote polecat leaves no session on the worker |
| 4 (optional) | Adoption by a new pane: `adopt` on attach, `Start` skips beacon and nudges when adopting, forced repaint; a session-scoped stop message for `Stop`, then stop forwarding `SIGTERM` | ~500 lines, mostly `internal/polecat` | Start-sequence tests with a detached exec present; end-to-end on a real worker |

### Acceptance criteria

1. With an agent mid-task, block the worker's port on the orchestrator for two
   minutes. The agent's pid on the worker is unchanged, the pane never closes,
   the output produced meanwhile appears after the block is lifted, and the
   worker log contains no `canceling the attached agent` line.
2. `kill -9` the launcher. `discover` shows
   the session with `exec: detached` and an `idle_since` time; the agent pid is
   alive. After the detach timeout the agent is gone and the checkpoint ref has
   advanced; after the idle-session timeout the session is `orphaned` and a new
   `session_open` for another polecat is accepted at `-max-sessions 1`.
3. Agent exits with code 7 while the launcher is disconnected. On reconnect the
   launcher prints the remaining output and exits 7.
4. Produce more than the ring size while detached. On resume the launcher
   prints the lost-bytes notice with the correct count and the TUI is usable.
5. A new launcher against a worker built before this change, and an old
   launcher against a new worker, pass the existing exec-stream test suite.
6. A staged worker binary is not applied while a detached session exists and is
   applied within one idle-session timeout of the last stream closing.
7. Drop all packets between launcher and worker without closing either socket.
   Within 30s the worker reports `exec: detached` and the launcher prints its
   reconnecting status.
8. Run an agent that ignores `SIGTERM` and stop its polecat session. The agent's
   process group is gone from the worker within `execWaitDelay` of the stop.
9. With the proxy unreachable for longer than the dead-man window and a native
   agent writing a tracked file continuously, the final checkpoint commit is
   taken after the agent's exit is logged.

## 8. Open questions

1. **Should a new pane adopt a detached agent, or replace it?** Adoption saves
   the agent when the launcher itself dies, which the incident in §1.1 was. It
   requires `Start` to know it adopted, so that it does not send a beacon or
   startup nudges to a working agent, and a rule for when a start means "new
   work" and must not adopt. *Recommendation:* ship replace (phases 1–3), then
   decide phase 4 on whether launcher death remains a meaningful share of
   drops.
2. **Which launcher signals end the agent?** *Recommendation:* stop forwarding
   `SIGHUP` in phase 2; keep forwarding `SIGTERM`/`SIGQUIT` until intentional
   kill paths (`Stop`, nuke, the plugin's `tmux kill-session`) call an explicit
   stop, then drop those too. Until then an orchestrator reboot may still end
   agents (failure mode 5). While they are forwarded they arm a worker-side
   kill (§3.3), so `Stop` keeps a hard bound.
3. **Timer defaults.** Launcher window 10 min, detach timeout 15 min,
   idle-session timeout 30 min, against a 20 min dead-man window at defaults.
   *Recommendation:* these values, as worker flags; revisit after a week of
   `idle_since` data. The ping interval (10s) and dead-stream threshold (30s)
   are constants, not flags.
4. **Replay fidelity for TUIs.** Lossless resume should need no repaint; a gap
   relies on the resize repaint. This is not verified against the agent TUIs
   in use. *Recommendation:* test in phase 2 before fixing the ring size;
   if repaint is unreliable, send a clear-screen before live output on any gap.
5. **Bind a session to the opening connection's TLS key?** It would stop one
   orchestrator acting on another's sessions on a shared worker, at the cost of
   stranding sessions when the orchestrator's certificate is re-issued.
   *Recommendation:* no; keep one orchestrator per worker as the stated
   assumption and record the peer key fingerprint in the worker log on attach.
6. **Worker-restart survival.** Out of scope here. *Recommendation:* a separate
   spec if needed, built on a per-session holder process; also correct socket
   spec §7, whose re-adoption text does not match the code.
7. **Could not determine from the code:** how the witness carries out
   `RESTART_POLECAT` (it is an agent acting on mail), and so whether any
   restart path tears the worker session down first; and whether the dead-man
   switch was meant to include control-connection pings as socket §7 says (the
   code feeds it from checkpoint pushes and `ls-remote` probes only,
   `supervisor.go:132-151`). *Recommendation:* confirm the first by reading a
   witness restart transcript before phase 3's sweep is written; treat the
   second as a doc correction.
