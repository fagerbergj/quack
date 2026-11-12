import type { NewsItem, PlayerFact } from './lineup/facts.ts';

export type Json = any;

/** Read-only Sleeper data the jobs depend on; the HTTP adapter is `createSleeperClient`. */
export type SleeperClient = {
  state(): Promise<Json>;
  league(id: string): Promise<Json>;
  users(id: string): Promise<Json>;
  rosters(id: string): Promise<Json>;
  matchups(id: string, week: number): Promise<Json>;
  draftPicks(id: string): Promise<Json>;
  winnersBracket(id: string): Promise<Json>;
  seasonStats(season: string): Promise<Json>;
  projections(season: string, week: number): Promise<Json>;
  schedule(season: string): Promise<Json>;
  scores(season: string, week: number): Promise<Json>;
  players(): Promise<Json>;
  trending(type: 'add' | 'drop', hours: number, limit: number): Promise<Json>;
  stats(season: string, week: number): Promise<Json>;
  research(season: string, week: number): Promise<Json>;
};

/** The slice of a player a news search needs. */
export type Searchable = Pick<PlayerFact, 'name' | 'team' | 'pos'>;

/** Whether a report is about the player rather than naming him in news about someone else; undefined when it cannot tell. */
export type SubjectCheck = (p: Searchable, n: NewsItem) => Promise<boolean | undefined>;

/** Dated news for one player up to `at`, kept only when it is about him (`about` decides past the title); the Exa adapter is `exaNews`. A failed search yields none. */
export type NewsSearch = (p: Searchable, at: number, about?: SubjectCheck) => Promise<NewsItem[]>;
