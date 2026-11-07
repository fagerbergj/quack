// Local models only, through llama-swap; nothing here can reach a hosted provider.
const BASE = 'http://100.64.0.6:11436/v1';
export const READER = 'jaison/qwen3.8-27b';
export const EMBEDDER = 'jaison/qwen3-embed';
// vLLM behind llama-swap on this host (jaison); prod shares it, so wait until it is nearly idle.
const METRICS = 'http://127.0.0.1:9993/metrics';

export const usage = { chat: 0, embed: 0, chatSeconds: 0, embedSeconds: 0, waits: 0 };

export async function waitForSlots(max = 2) {
  for (;;) {
    const text = await fetch(METRICS).then((r) => r.text(), () => '');
    const running = Number(/^vllm:num_requests_running\{[^}]*\} (\S+)/m.exec(text)?.[1] ?? 0);
    if (running <= max) return;
    usage.waits++;
    await new Promise((r) => setTimeout(r, 15_000));
  }
}

async function post(path: string, body: object) {
  const res = await fetch(`${BASE}${path}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
  if (!res.ok) throw new Error(`${path} ${res.status}: ${await res.text()}`);
  return res.json();
}

/** One chat call, thinking off and temperature 0 so every arm's reader behaves the same. */
export async function chat(system: string, user: string, maxTokens = 300): Promise<string> {
  const t = Date.now();
  const j = await post('/chat/completions', {
    model: READER, temperature: 0, max_tokens: maxTokens,
    messages: [{ role: 'system', content: system }, { role: 'user', content: user }],
    chat_template_kwargs: { enable_thinking: false },
  });
  usage.chat++;
  usage.chatSeconds += (Date.now() - t) / 1000;
  return String(j.choices[0].message.content ?? '').trim();
}

export async function embed(texts: string[]): Promise<number[][]> {
  const out: number[][] = [];
  // The embedder runs --parallel 4 on CPU; small batches keep each request short.
  for (let i = 0; i < texts.length; i += 16) {
    const t = Date.now();
    const j = await post('/embeddings', { model: EMBEDDER, input: texts.slice(i, i + 16) });
    usage.embed++;
    usage.embedSeconds += (Date.now() - t) / 1000;
    out.push(...j.data.map((d: { embedding: number[] }) => d.embedding));
  }
  return out;
}
