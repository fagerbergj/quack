// Runs inside quack-v1 (read-only, sent as base64): for each session key in env KEYS (base64 of a JSON array), prints one JSON line per pi.tool task and per workflow task with its name and start/end in ms.
const { DatabaseSync } = require('node:sqlite');
const fs = require('node:fs');
const dirs = new Map();
for (const d of fs.readdirSync('/data')) {
  try { dirs.set(fs.readFileSync(`/data/${d}/session-key`, 'utf8').trim(), d); } catch {}
}
for (const key of JSON.parse(Buffer.from(process.env.KEYS, 'base64').toString())) {
  const d = dirs.get(key);
  if (!d) { console.log(JSON.stringify({ key, missing: true })); continue; }
  const db = new DatabaseSync(`/data/${d}/session.sqlite`, { readOnly: true });
  const names = new Map();
  for (const e of db.prepare('select id, record from entries').all()) {
    let rec;
    try { rec = JSON.parse(e.record); } catch { continue; }
    for (const m of rec.model ?? []) for (const c of Array.isArray(m.content) ? m.content : []) if (c.type === 'toolCall') names.set(c.id, c.name);
  }
  const gens = new Map();
  for (const t of db.prepare("select id, record from tasks where kind = '\"pi.generation\"'").all()) { const r = JSON.parse(t.record); gens.set(r.id, r); }
  for (const t of db.prepare("select id, record from tasks where kind = '\"pi.tool\"'").all()) {
    const r = JSON.parse(t.record);
    const g = gens.get(r.owner);
    console.log(JSON.stringify({ key, id: r.id, name: names.get(r.input?.callId) ?? '?', startedAt: r.startedAt, endedAt: r.endedAt, genStartedAt: g?.startedAt, genEndedAt: g?.endedAt }));
  }
  for (const t of db.prepare("select record from tasks where kind like '%workflow%'").all()) {
    const r = JSON.parse(t.record);
    console.log(JSON.stringify({ key, id: r.id, name: `workflow:${JSON.parse(JSON.stringify(r.kind))}`, startedAt: r.startedAt, endedAt: r.endedAt }));
  }
}
