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
