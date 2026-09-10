// Minimal MCP streamable-HTTP client (plain fetch, no SDK). Used by the shim
// (tools/list at session start) and by the generated pi extension (tools/call).
let nextId = 1;

function parseBody(text, contentType) {
  if (!contentType.includes("text/event-stream")) return JSON.parse(text);
  // take the first data: line carrying a JSON-RPC response
  for (const line of text.split("\n")) {
    if (line.startsWith("data:")) {
      const msg = JSON.parse(line.slice(5).trim());
      if (msg.id !== undefined || msg.error) return msg;
    }
  }
  throw new Error("no JSON-RPC message in SSE body");
}

export class McpClient {
  constructor(url) {
    this.url = url;
    this.sessionId = null;
  }

  async rpc(method, params, notification = false) {
    const body = { jsonrpc: "2.0", method, params };
    if (!notification) body.id = nextId++;
    const headers = {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
    };
    if (this.sessionId) headers["mcp-session-id"] = this.sessionId;
    const res = await fetch(this.url, { method: "POST", headers, body: JSON.stringify(body) });
    if (!res.ok && res.status !== 202) throw new Error(`mcp ${method}: HTTP ${res.status}`);
    this.sessionId = res.headers.get("mcp-session-id") || this.sessionId;
    if (notification || res.status === 202) return null;
    const msg = parseBody(await res.text(), res.headers.get("content-type") || "");
    if (msg.error) throw new Error(`mcp ${method}: ${msg.error.message}`);
    return msg.result;
  }

  async connect() {
    await this.rpc("initialize", {
      protocolVersion: "2025-03-26",
      capabilities: {},
      clientInfo: { name: "pi-acp", version: "0" },
    });
    await this.rpc("notifications/initialized", {}, true);
  }

  toolsList() {
    return this.rpc("tools/list", {});
  }

  toolsCall(name, args) {
    return this.rpc("tools/call", { name, arguments: args || {} });
  }
}

// Permission policy - the pi translation of the opencode config quack
// generates (serve.go opencodeEnv): hard denies never leave the process,
// "ask" escalates to quack's safety judge via the shim's loopback endpoint.
const DENY = [/^git push(\s|$)/, /^git clone(\s|$)/, /^gh repo clone(\s|$)/];
const ENV_FILE = /(^|\/)[^/]*\.env(\.[^/]*)?$/;

export function checkPolicy(toolName, input = {}) {
  if (toolName === "bash") {
    const cmd = (input.command || "").trim();
    if (DENY.some((re) => re.test(cmd))) return { block: `denied by policy: ${cmd.split(" ").slice(0, 3).join(" ")} (delivery is gate-owned)` };
  }
  if (toolName === "read" && ENV_FILE.test(input.path || "")) return { ask: `read ${input.path}` };
  return null;
}

// Deterministic tool-call-loop guard - the pi/ACP twin of
// internal/tools/repeatguard.go's repeat guard, for quack's MCP-bridged
// tools (quackmcp_*, registered by pi-acp.mjs's extension). Same rule:
// refuse the threshold'th+ consecutive identical call, then end the round
// once the model has ignored that refusal hardStopAfter times more.
export const DEFAULT_LOOP_THRESHOLD = 3;
export const DEFAULT_LOOP_HARD_STOP_AFTER_REFUSALS = 2;

// lastCall: the single last-call slot for this process (mirrors Go's
// repeatStates - one slot, not a map per fingerprint), so a DIFFERENT call
// resets the streak instead of a fingerprint accumulating across unrelated
// calls. The pinned pi subprocess lives for the node's whole session, not
// just one round, so this state persists across rounds too.
let lastCall = null;

function loopFingerprint(name, args) {
  return name + ":" + JSON.stringify(args ?? {});
}

// checkLoop returns null to run normally, {refuse} to short-circuit without
// executing, or {stop} once the model repeats an already-refused call.
export function checkLoop(name, args, threshold = DEFAULT_LOOP_THRESHOLD, hardStopAfter = DEFAULT_LOOP_HARD_STOP_AFTER_REFUSALS) {
  const fp = loopFingerprint(name, args);
  if (!lastCall || lastCall.fingerprint !== fp) {
    lastCall = { fingerprint: fp, count: 0 };
  }
  lastCall.count += 1;
  if (lastCall.count > threshold + hardStopAfter) {
    return { stop: `tool-call loop: ${name} called with identical arguments ${lastCall.count} consecutive times despite being refused; node terminated` };
  }
  if (lastCall.count >= threshold) {
    return {
      refuse: `REFUSED: this is the ${lastCall.count}th consecutive time you issued this exact ${name} call with these exact arguments. ` +
        `Its result has not changed - it is already in the conversation above. Re-issuing it again will END THIS NODE'S TURN as a ` +
        `failure. Take a DIFFERENT action: use the result you already have, try a different tool or different arguments, or if you ` +
        `are finished, stop calling tools and write your final answer now.`,
    };
  }
  return null;
}
