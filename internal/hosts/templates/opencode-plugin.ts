// graft-owned: written by `graft init`, removed by `graft uninstall`.
/**
 * graft for opencode: repository micro-skills.
 *
 * opencode has no command hooks, so this plugin forwards the events graft
 * learns from to `graft _hook <sub>` with a Claude-style JSON payload marked
 * `graft_host: "opencode"`, and adds the facts graft returns to the session's
 * system prompt:
 *
 *   session start (1.x `session.created`, 2.x first prompt) -> session-start
 *   shell `tool.execute.after` (`bash` on 1.x, `shell` on 2.x) -> tool-savings
 *   turn end (1.x `session.idle`, 2.x `session.execution.*`)  -> stop
 *
 * It fails open: when graft is missing, fails or runs past five seconds, the
 * plugin changes nothing. GRAFT_NO_MICROSKILLS turns it off.
 */
import { spawnSync } from "node:child_process";

const TIMEOUT_MS = 5_000;

/** One graft hook call; the context it returns, "" on any failure. */
function graftHook(sub: string, payload: Record<string, unknown>): string {
  if (process.env.GRAFT_NO_MICROSKILLS) return "";
  try {
    const res = spawnSync("graft", ["_hook", sub], {
      input: JSON.stringify({ ...payload, graft_host: "opencode" }),
      encoding: "utf8",
      timeout: TIMEOUT_MS,
      stdio: ["pipe", "pipe", "ignore"],
    });
    if (res.status !== 0 || !res.stdout) return "";
    const out = JSON.parse(res.stdout.trim().split("\n").pop() || "{}");
    return String(out?.hookSpecificOutput?.additionalContext ?? "");
  } catch {
    return "";
  }
}

const textOf = (content: any): string =>
  typeof content === "string"
    ? content
    : Array.isArray(content)
      ? content.map((c: any) => (typeof c === "string" ? c : c?.text ?? "")).filter(Boolean).join("\n")
      : "";

/** The per-process session state both opencode versions share. */
function graftSessions(directory: string) {
  const facts = new Map<string, string>();
  const started = new Set<string>();
  return {
    start(id: string) {
      if (!id || started.has(id)) return;
      started.add(id);
      const text = graftHook("session-start", { session_id: id, cwd: directory, hook_event_name: "SessionStart", source: "startup" });
      if (text) facts.set(id, text);
    },
    facts(id: string): string {
      return facts.get(id) ?? "";
    },
    tool(id: string, callID: string, tool: string, input: any, text: string, exit: number | undefined, failed: boolean) {
      // opencode 1.x reports the shell tool as "bash", 2.x as "shell".
      if (!id || (tool !== "bash" && tool !== "shell")) return;
      graftHook("tool-savings", {
        session_id: id,
        cwd: directory,
        hook_event_name: "PostToolUse",
        tool_name: "Bash",
        tool_use_id: callID,
        tool_input: input ?? {},
        tool_response: { content: text, isError: failed, exit_code: exit },
      });
    },
    stop(id: string) {
      if (id) graftHook("stop", { session_id: id, cwd: directory, hook_event_name: "Stop" });
    },
  };
}

/** opencode 1.x. */
const server = async ({ directory }: any) => {
  const graft = graftSessions(directory ?? process.cwd());
  return {
    event: async ({ event }: any) => {
      const props = event?.properties ?? {};
      if (event?.type === "session.created" && !props.info?.parentID) graft.start(props.info?.id ?? props.sessionID);
      else if (event?.type === "session.idle") graft.stop(props.sessionID);
    },
    "experimental.chat.system.transform": async (input: any, output: any) => {
      const text = graft.facts(input?.sessionID);
      if (text && Array.isArray(output?.system)) output.system.push(text);
    },
    "tool.execute.after": async (input: any, output: any) => {
      const exit = typeof output?.metadata?.exit === "number" ? output.metadata.exit : undefined;
      graft.tool(input?.sessionID, input?.callID, input?.tool, input?.args, String(output?.output ?? ""), exit, exit !== undefined && exit !== 0);
    },
  };
};

/** opencode 2.x. */
const setup = async (ctx: any) => {
  const graft = graftSessions(ctx?.location?.directory ?? ctx?.location?.path ?? process.cwd());
  try {
    await ctx.session.hook("prompt", async (e: any) => graft.start(e?.sessionID));
    await ctx.session.hook("context", (e: any) => {
      const text = graft.facts(e?.sessionID);
      if (text && Array.isArray(e?.system)) e.system.push({ type: "text", text });
    });
    await ctx.tool.hook("execute.after", async (e: any) => {
      const ok = e?.status === "completed";
      const result = ok ? e.result : undefined;
      const text = ok ? textOf(result?.content) || String(result?.output ?? "") : String(e?.error?.message ?? e?.error ?? "");
      const exit = typeof result?.metadata?.exit === "number" ? result.metadata.exit : undefined;
      graft.tool(e?.sessionID, e?.id, e?.tool, e?.input, text, exit, !ok || (exit !== undefined && exit !== 0));
    });
  } catch {
    return;
  }
  (async () => {
    try {
      for await (const ev of ctx.event.subscribe()) {
        const props = ev?.properties ?? ev?.data ?? ev;
        if (/^session\.execution\.(succeeded|failed|interrupted)$/.test(String(ev?.type ?? ""))) graft.stop(props?.sessionID);
      }
    } catch {
      /* fail open */
    }
  })();
};

// One default export: opencode 1.x reads `server`, 2.x reads `setup`.
export default { id: "graft", server, setup };
