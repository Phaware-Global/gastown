// Gas Town OpenCode plugin: hooks SessionStart/Compaction via events.
// Injects gt prime context into the system prompt via the session "context" hook.
//
// OpenCode 2.x plugin API: the module's default export is a definition with an
// id and a setup function. One shared OpenCode service loads this file once per
// location (the directory that holds .opencode/plugins), so the setup context,
// not the process environment, says which session directory this instance
// serves; process.env belongs to whichever session started the service.
import { execFile } from "node:child_process";

export default {
  id: "gastown",
  async setup(ctx) {
    const directory = ctx?.location?.directory || process.cwd();
    const gtBin = process.env.GT_BIN || "{{GT_BIN}}";
    const townRoot = process.env.GT_ROOT || "";

    // Promise-based context loading ensures the context hook can await the
    // result even when several requests race at session start.
    let primePromise = null;
    // Sessions this location has served, so the server-wide event stream only
    // triggers work for our own sessions.
    const ownSessions = new Set();

    const outputTail = (value, maxChars = 2000) => {
      if (value === undefined || value === null) return "<empty>";
      let text;
      if (typeof value === "string") {
        text = value;
      } else if (value instanceof Uint8Array) {
        text = new TextDecoder().decode(value);
      } else {
        text = String(value);
      }
      text = text.trimEnd();
      if (!text) return "<empty>";
      if (text.length <= maxChars) return text;
      return `...<truncated ${text.length - maxChars} chars>\n${text.slice(-maxChars)}`;
    };

    const exitCode = (err) => {
      const candidates = [err?.exitCode, err?.exit_code, err?.status, err?.code];
      for (const code of candidates) {
        if (typeof code === "number") return code;
      }
      return null;
    };

    const shellQuote = (value) => `'${String(value).replace(/'/g, `'\\''`)}'`;

    const gtCommand = () => {
      if (/^[A-Za-z0-9_./-]+$/.test(gtBin)) return gtBin;
      return shellQuote(gtBin);
    };

    const isDoltBackedCommand = (cmd) =>
      /(^|\s)(?:'[^']*\/|[^'\s]*\/)?(?:gt|bd)'?\s/.test(cmd) &&
      !/(^|\s)(?:'[^']*\/|[^'\s]*\/)?gt'?\s+dolt\s+status(\s|$)/.test(cmd);

    // process.env is the shared service's environment, which carries the
    // identity of whichever session started it (GT_ROLE, BD_ACTOR, ...).
    // Drop those so gt derives the role from the working directory, as it
    // does for a session with no GT_ROLE; town-level settings stay.
    const sessionScopedEnv =
      /^(GT_(ROLE|RIG|SESSION|SESSION_ID|AGENT|AGENT_READY|REVIEWER|REVIEWER_GITHUB_TOKEN|RUN|POLECAT|HOOK_SOURCE|WORK_[A-Z_]+)|BD_ACTOR|GIT_AUTHOR_NAME|GIT_COMMITTER_NAME)$/;
    const childEnv = () => {
      const env = {};
      for (const [key, value] of Object.entries(process.env)) {
        if (!sessionScopedEnv.test(key)) env[key] = value;
      }
      return env;
    };

    // Runs cmd through /bin/sh in this location's directory. Resolves to stdout;
    // rejects with the error carrying stdout/stderr tails and the exit code.
    const sh = (cmd) =>
      new Promise((resolve, reject) => {
        execFile(
          "/bin/sh",
          ["-lc", cmd],
          { cwd: directory, env: childEnv(), maxBuffer: 64 * 1024 * 1024 },
          (err, stdout, stderr) => {
            if (err) {
              err.stdout = stdout;
              err.stderr = stderr;
              reject(err);
              return;
            }
            resolve(stdout);
          },
        );
      });

    const captureDoltStatus = async () => {
      const statusCmd = `timeout 10s ${gtCommand()} dolt status 2>&1`;
      try {
        return await sh(statusCmd);
      } catch (err) {
        return [
          `status_command: ${statusCmd}`,
          `status_error: ${err?.message || err}`,
          `status_stdout_tail: ${outputTail(err?.stdout)}`,
          `status_stderr_tail: ${outputTail(err?.stderr)}`,
        ].join("\n");
      }
    };

    const logFailure = async (cmd, err) => {
      const code = exitCode(err);
      const message = err?.message || String(err);
      const timeout = code === 124 || /exit code 124|timed?\s*out/i.test(message)
        ? "yes (exit code 124 / timeout)"
        : "not indicated";
      const lines = [
        "[gastown] command failed",
        `command: ${cmd}`,
        `exit_code: ${code ?? "unknown"}`,
        `timeout: ${timeout}`,
        `error: ${message}`,
        `stdout_tail: ${outputTail(err?.stdout)}`,
        `stderr_tail: ${outputTail(err?.stderr)}`,
      ];
      if (isDoltBackedCommand(cmd)) {
        lines.push(`dolt_status_tail:\n${outputTail(await captureDoltStatus())}`);
        lines.push(
          "suggested_recovery: If Dolt is unhealthy or another gt/bd command is hanging, capture SIGQUIT and `gt dolt status` diagnostics before escalating; otherwise retry after the timeout clears."
        );
      } else {
        lines.push("suggested_recovery: Inspect the command, stdout/stderr tails, and retry once the timeout or process failure is resolved.");
      }
      console.error(lines.join("\n"));
    };

    // simpleRole reduces a role path (mayor, deacon, <rig>/polecats/<name>,
    // <rig>/witness, ...) to its display name.
    const simpleRole = (value) => {
      if (!value) return "";
      const parts = value.split("/").filter(Boolean);
      if (parts.length >= 2 && parts[1] === "polecats") return "polecat";
      if (parts.length >= 2 && parts[1] === "crew") return "crew";
      if (parts[0] === "mayor" || parts[0] === "deacon" || parts[0] === "assistant") return parts[0];
      if (parts.length >= 2) return parts[1];
      return parts[0];
    };

    // The role path comes from this location's directory under the town root.
    // GT_ROLE in process.env names the session that started the shared service.
    const rolePath = () => {
      if (townRoot && directory.startsWith(townRoot + "/")) {
        return directory.slice(townRoot.length + 1);
      }
      return "";
    };

    const eventSessionID = (event) =>
      event?.properties?.info?.id || event?.properties?.sessionID || event?.sessionID || "";

    const captureRun = async (cmd) => {
      try {
        return await sh(cmd);
      } catch (err) {
        await logFailure(cmd, err);
        return "";
      }
    };

    const loadPrime = async (source = "startup", sessionID = "") => {
      const env = [`GT_HOOK_SOURCE=${shellQuote(source)}`];
      if (sessionID) {
        env.push(`GT_SESSION_ID=${shellQuote(sessionID)}`);
      }
      // NOTE: session-started nudge to deacon removed — it interrupted
      // the deacon's await-signal backoff. Deacon wakes on beads activity.
      return captureRun(`${env.join(" ")} ${gtCommand()} prime --hook`);
    };

    await ctx.session.hook("context", async (event) => {
      const sessionID = event?.sessionID || "";
      if (sessionID) ownSessions.add(sessionID);
      if (!primePromise) {
        primePromise = loadPrime("startup", sessionID);
      }
      const context = await primePromise;
      if (context) {
        event.system.push({ type: "text", text: context });
      } else {
        // Reset so the next request retries instead of pushing empty forever.
        primePromise = null;
      }
    });

    await ctx.session.hook("compaction", async (event) => {
      if (!Array.isArray(event?.system)) return;
      const roleDisplay = simpleRole(rolePath()) || "unknown";
      event.system.push({
        type: "text",
        text: `
## Gas Town Multi-Agent System

Keep these instructions in the summary, verbatim:
**After Compaction:** Run \`gt prime --hook\` to restore full context.
**Check Hook:** \`gt hook\` - if work present, execute immediately (GUPP).
**Role:** ${roleDisplay}
`,
      });
    });

    // The event stream is server-wide; act only on sessions this location served.
    const controller = new AbortController();
    (async () => {
      try {
        for await (const event of ctx.event.subscribe({ signal: controller.signal })) {
          const sessionID = eventSessionID(event);
          if (!sessionID || !ownSessions.has(sessionID)) continue;
          if (event?.type === "session.compacted") {
            // Reset so the next context hook gets fresh context.
            primePromise = loadPrime("compact", sessionID);
          } else if (event?.type === "session.deleted") {
            ownSessions.delete(sessionID);
            await captureRun(`${gtCommand()} costs record --session ${shellQuote(sessionID)}`);
          }
        }
      } catch (err) {
        if (!controller.signal.aborted) {
          console.error(`[gastown] event stream ended: ${err?.message || err}`);
        }
      }
    })();

    return () => controller.abort();
  },
};
