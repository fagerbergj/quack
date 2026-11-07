// The `episodes` arm: raw episode passages in SQLite, recalled by FTS5 BM25 and cosine, fused by reciprocal rank.
import { DatabaseSync } from 'node:sqlite';
import type { Episode } from './episodes.ts';
import { embed } from './llm.ts';

const PASSAGE_CHARS = 1200;
export const tokens = (s: string) => Math.ceil(s.length / 4);

/** Cuts an episode at its sections, then packs paragraphs up to PASSAGE_CHARS; each passage names its run and section. */
export function passages(e: Episode) {
  const out: { section: string; text: string }[] = [];
  const head = `${e.key.replace('github:fagerbergj/quack', '')} ${e.created.slice(0, 16)}Z "${e.title.slice(0, 80)}"`;
  for (const sec of e.text.split(/\n(?=### )/).slice(1)) {
    const [title, ...rest] = sec.split('\n');
    let buf = '';
    const flush = () => buf.trim() && out.push({ section: title.slice(4), text: `[${head} | ${title.slice(4)}]\n${buf.trim()}` });
    for (const para of rest.join('\n').split(/\n\n+/)) {
      if (buf && buf.length + para.length > PASSAGE_CHARS) (flush(), (buf = ''));
      buf += `${para.slice(0, PASSAGE_CHARS * 2)}\n\n`;
    }
    flush();
  }
  return out;
}

const cosine = (a: Float32Array, b: Float32Array) => {
  let dot = 0, na = 0, nb = 0;
  for (let i = 0; i < a.length; i++) (dot += a[i] * b[i]), (na += a[i] * a[i]), (nb += b[i] * b[i]);
  return dot / Math.sqrt(na * nb);
};

export async function build(episodes: Episode[]) {
  const db = new DatabaseSync(':memory:');
  db.exec(`create table p (id integer primary key, created text, text text, emb blob);
           create virtual table f using fts5(text, content='p', content_rowid='id');`);
  const rows = episodes.flatMap((e) => passages(e).map((p) => ({ created: e.created, text: p.text })));
  const vecs = await embed(rows.map((r) => r.text));
  const ins = db.prepare('insert into p (created, text, emb) values (?, ?, ?)');
  rows.forEach((r, i) => ins.run(r.created, r.text, new Uint8Array(new Float32Array(vecs[i]).buffer)));
  db.exec(`insert into f(rowid, text) select id, text from p`);
  return { db, passages: rows.length };
}

// shortcut: linear cosine scan over every row, fine to about 50k rows; move to an index past that.
export async function recall(db: DatabaseSync, query: string, capTokens: number) {
  const terms = [...new Set(query.toLowerCase().match(/[a-z0-9_.#/-]{3,}/g) ?? [])].map((t) => `"${t.replaceAll('"', '')}"`);
  const bm25 = terms.length ? db.prepare(`select rowid id from f where f match ? order by rank limit 30`).all(terms.join(' OR ')) as { id: number }[] : [];
  const [q] = await embed([query]);
  const qv = new Float32Array(q);
  const all = db.prepare('select id, emb from p').all() as { id: number; emb: Uint8Array }[];
  const dense = all.map((r) => ({ id: r.id, s: cosine(qv, new Float32Array(r.emb.buffer, r.emb.byteOffset, r.emb.byteLength / 4)) })).sort((a, b) => b.s - a.s).slice(0, 30);
  const score = new Map<number, number>();
  for (const list of [bm25.map((r) => r.id), dense.map((r) => r.id)]) list.forEach((id, rank) => score.set(id, (score.get(id) ?? 0) + 1 / (60 + rank)));
  const picked: { id: number; text: string }[] = [];
  let used = 0;
  for (const [id] of [...score].sort((a, b) => b[1] - a[1])) {
    const row = db.prepare('select id, text from p where id = ?').get(id) as { id: number; text: string };
    if (used + tokens(row.text) > capTokens) continue;
    picked.push(row);
    used += tokens(row.text);
  }
  // Rows go in oldest run first, so rendering by id puts a later value after the earlier one.
  return picked.sort((a, b) => a.id - b.id).map((r) => r.text);
}
