// Runs inside quack-v1 (base64, read-only): prints task kinds and a sample of non-generation, non-tool tasks for the session in env KEY.
const { DatabaseSync } = require('node:sqlite');
const fs = require('node:fs');
const key = process.env.KEY;
const dir = fs.readdirSync('/data').find((d) => { try { return fs.readFileSync(`/data/${d}/session-key`, 'utf8').trim() === key; } catch { return false; } });
const db = new DatabaseSync(`/data/${dir}/session.sqlite`, { readOnly: true });
console.log(JSON.stringify(db.prepare('select kind, count(*) n from tasks group by kind').all()));
for (const t of db.prepare("select record from tasks where kind not in ('\"pi.generation\"', '\"pi.tool\"') limit 6").all()) console.log(t.record.slice(0, 600));
