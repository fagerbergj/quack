// Runs inside the quack-v1 container (sent over ssh stdin-less as base64, nothing written to disk): reads one JSON request per line, posts it to Clef one at a time, writes one JSON line back.
const URL_ = 'http://llm-swap:11436/upstream/clef/v1/systemone';
let buf = '';
let chain = Promise.resolve();
process.stdin.on('data', (d) => {
  buf += d;
  let i;
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i);
    buf = buf.slice(i + 1);
    if (!line.trim()) continue;
    const m = JSON.parse(line);
    // A message flagged par runs at once, for the concurrency measurement; the rest queue.
    if (m.par) one(m);
    else chain = chain.then(() => one(m));
  }
});
async function one({ id, body }) {
  const t = Date.now();
  let out;
  try {
    const r = await fetch(URL_, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
    out = { id, status: r.status, ms: Date.now() - t, json: await r.json().catch(() => null) };
  } catch (e) {
    out = { id, status: 0, ms: Date.now() - t, error: String(e) };
  }
  process.stdout.write(`${JSON.stringify(out)}\n`);
}
