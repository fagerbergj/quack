import { DatabaseSync } from 'node:sqlite';

export type SnapshotRow = { league: string; date: string; takenAt: string; data: unknown };

/** League store on the service's volume (SL-5): snapshots are kept indefinitely. Suggestions, outcomes and prediction scores are schema only for now. */
export function openStore(path: string) {
  const db = new DatabaseSync(path);
  db.exec(`
    CREATE TABLE IF NOT EXISTS snapshots (league TEXT NOT NULL, date TEXT NOT NULL, taken_at TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY (league, date));
    CREATE TABLE IF NOT EXISTS suggestions (id TEXT PRIMARY KEY, league TEXT NOT NULL, kind TEXT NOT NULL, created TEXT NOT NULL, body TEXT NOT NULL);
    CREATE TABLE IF NOT EXISTS outcomes (suggestion TEXT NOT NULL REFERENCES suggestions(id), recorded TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY (suggestion, recorded));
    CREATE TABLE IF NOT EXISTS prediction_scores (id TEXT PRIMARY KEY, league TEXT NOT NULL, week INTEGER NOT NULL, scored TEXT NOT NULL, body TEXT NOT NULL);
  `);
  const insert = db.prepare('INSERT OR IGNORE INTO snapshots (league, date, taken_at, data) VALUES (?, ?, ?, ?)');
  type Raw = { league: string; date: string; taken_at: string; data: string };
  const row = (r: Raw): SnapshotRow => ({ league: r.league, date: r.date, takenAt: r.taken_at, data: JSON.parse(r.data) });

  return {
    /** Stores the first snapshot of a day; a later one for the same day changes nothing. Returns whether it was stored. */
    saveSnapshot(league: string, date: string, takenAt: string, data: unknown): boolean {
      return insert.run(league, date, takenAt, JSON.stringify(data)).changes > 0;
    },
    snapshot(league: string, date: string): SnapshotRow | undefined {
      const r = db.prepare('SELECT * FROM snapshots WHERE league = ? AND date = ?').get(league, date) as Raw | undefined;
      return r && row(r);
    },
    latest(league: string): SnapshotRow | undefined {
      const r = db.prepare('SELECT * FROM snapshots WHERE league = ? ORDER BY date DESC LIMIT 1').get(league) as Raw | undefined;
      return r && row(r);
    },
    /** Dates only, newest first; a snapshot is fetched by date. */
    history(league: string): Array<{ date: string; takenAt: string }> {
      const rows = db.prepare('SELECT date, taken_at FROM snapshots WHERE league = ? ORDER BY date DESC').all(league) as Array<{ date: string; taken_at: string }>;
      return rows.map((r) => ({ date: r.date, takenAt: r.taken_at }));
    },
    close: () => db.close(),
  };
}

export type Store = ReturnType<typeof openStore>;
