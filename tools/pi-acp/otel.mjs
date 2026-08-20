// Minimal OTLP/HTTP JSON trace exporter (plain fetch, no SDK). Spans parent
// under the TRACEPARENT env quack's round span exports (proc.go
// traceparentEnv); attribute names mirror internal/inference/emit.go +
// internal/otelobs/logs.go so dashboards see one vocabulary.
import { randomBytes } from "node:crypto";

const TRUNC = 8192;
const cut = (s) => (s.length > TRUNC ? s.slice(0, TRUNC) + "…[truncated]" : s);
const nowNs = () => (BigInt(Date.now()) * 1000000n).toString();
const attr = (key, v) =>
  typeof v === "number"
    ? { key, value: Number.isInteger(v) ? { intValue: String(v) } : { doubleValue: v } }
    : { key, value: { stringValue: cut(String(v)) } };

// Mirrors quack's Go convention (otelobs.signalURL, #814): an endpoint that
// already names a path is the full signal URL; only a bare host gets /v1/traces.
function signalURL(endpoint) {
  const t = endpoint.replace(/\/+$/, "");
  const rest = t.includes("://") ? t.slice(t.indexOf("://") + 3) : t;
  return rest.includes("/") ? endpoint : t + "/v1/traces";
}

export class Otel {
  constructor(model) {
    this.model = model || "unknown";
    this.endpoint = process.env.OTEL_EXPORTER_OTLP_ENDPOINT || "";
    const m = /^00-([0-9a-f]{32})-([0-9a-f]{16})-/.exec(process.env.TRACEPARENT || "");
    this.traceId = m ? m[1] : randomBytes(16).toString("hex");
    this.parentId = m ? m[2] : undefined;
    this.enabled = !!this.endpoint;
    this.buf = [];
    this.gen = null;          // open generation span
    this.tools = new Map();   // open tool spans by toolCallId
  }

  genStart() {
    if (!this.enabled) return;
    this.gen = { start: nowNs(), thinking: "" };
  }

  addThinking(delta) {
    if (this.gen && this.gen.thinking.length < TRUNC) this.gen.thinking += delta;
  }

  // message: pi's AgentMessage from message_end; usage: last cumulative usage seen.
  genEnd(message, usage) {
    if (!this.enabled || !this.gen) return;
    const u = message?.usage ?? usage ?? {};
    const attrs = [
      attr("gen_ai.operation.name", "chat"),
      attr("gen_ai.provider.name", "openai"),
      attr("gen_ai.request.model", this.model),
      attr("gen_ai.semconv.version", "1.41.0"),
    ];
    if (message?.content) attrs.push(attr("gen_ai.output.messages", JSON.stringify(message.content)));
    // Native emits a string array; keep the value type identical (emit.go).
    if (message?.stopReason)
      attrs.push({ key: "gen_ai.response.finish_reasons", value: { arrayValue: { values: [{ stringValue: String(message.stopReason) }] } } });
    if (u.input) attrs.push(attr("gen_ai.usage.input_tokens", u.input));
    if (u.output) attrs.push(attr("gen_ai.usage.output_tokens", u.output));
    if (this.gen.thinking) attrs.push(attr("quack.thinking", this.gen.thinking));
    this.push(`chat ${this.model}`, this.gen.start, attrs, false);
    this.gen = null;
  }

  toolStart(id, name, args) {
    if (!this.enabled) return;
    this.tools.set(id, { name, args, start: nowNs() });
  }

  toolEnd(id, resultText, isError) {
    const t = this.tools.get(id);
    if (!t) return;
    this.tools.delete(id);
    const attrs = [
      attr("gen_ai.operation.name", "execute_tool"),
      attr("gen_ai.tool.name", t.name),
      attr("gen_ai.tool.call.arguments", JSON.stringify(t.args ?? {})),
    ];
    if (resultText) attrs.push(attr("gen_ai.tool.call.result", resultText));
    this.push(`execute_tool ${t.name}`, t.start, attrs, isError);
  }

  push(name, startNs, attributes, isError) {
    this.buf.push({
      traceId: this.traceId,
      spanId: randomBytes(8).toString("hex"),
      parentSpanId: this.parentId,
      name,
      kind: 1,
      startTimeUnixNano: startNs,
      endTimeUnixNano: nowNs(),
      attributes,
      status: isError ? { code: 2 } : {},
    });
  }

  // Fire-and-forget: export failures never touch the RPC loop.
  async flush() {
    if (!this.enabled || this.buf.length === 0) return;
    const spans = this.buf.splice(0);
    const body = JSON.stringify({
      resourceSpans: [{
        resource: { attributes: [attr("service.name", process.env.OTEL_SERVICE_NAME || "pi-acp")] },
        scopeSpans: [{ scope: { name: "quack.acp.pi" }, spans }],
      }],
    });
    try {
      const res = await fetch(signalURL(this.endpoint), {
        method: "POST",
        headers: { "content-type": "application/json" },
        body,
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
    } catch (e) {
      console.error(`pi-acp: otlp export dropped ${spans.length} span(s): ${e.message}`);
    }
  }
}
