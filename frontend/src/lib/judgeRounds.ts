import type { ArtifactSummary } from '../api'

export const pct = (score: number) => `${Math.round(score * 100)}%`

export interface JudgeRoundRef {
  name: string
  round: number
}

// The node's judge_round artifacts saved inside [startedAt, endAt]. Their ids carry an execution id, not the turn
// id, so a node id reused across turns (a recurring extension job) is told apart by its run window.
// ponytail: a retry inside one window keeps only the latest execution's rounds.
export function nodeJudgeRounds(list: ArtifactSummary[], nodeId: string, startedAt: number, endAt: number): JudgeRoundRef[] {
  const hits = list
    .filter(a => a.kind === 'judge_round' && a.lineage?.node_id === nodeId)
    .map(a => ({ a, t: Date.parse(a.lineage?.saved_at ?? '') }))
    .filter(x => x.t >= startedAt && x.t <= endAt)
  if (hits.length === 0) return []
  const exec = (name: string) => name.slice(0, name.lastIndexOf(`-${nodeId}-`))
  const latest = exec(hits.reduce((m, x) => (x.t > m.t ? x : m)).a.name)
  return hits
    .filter(x => exec(x.a.name) === latest)
    .map(x => ({ name: x.a.name, round: x.a.lineage?.round ?? 0 }))
    .sort((p, q) => p.round - q.round)
}

export interface JudgeRoundDetail {
  round: number
  passed: boolean
  score?: number
  // failed: the criterion at the floor of a failed round - the one that sank it.
  criteria: { name: string; score?: number; feedback?: string; failed: boolean }[]
}

interface RawJudgeRound {
  round?: number
  passed?: boolean
  score?: number
  criteria?: { name?: string; score?: number; feedback?: string }[]
}

// Parses a judge_round body. The round score is its weakest criterion (vetting.aggregateVerdict), so a failed
// round failed on the criteria at that floor; the pass threshold is not in the record.
export function parseJudgeRound(body: unknown, fallbackRound: number): JudgeRoundDetail | null {
  if (body == null || typeof body !== 'object' || Array.isArray(body)) return null
  const b = body as RawJudgeRound
  const passed = b.passed === true
  const criteria = (b.criteria ?? [])
    .filter(c => typeof c?.name === 'string')
    .map(c => ({ name: c.name as string, score: typeof c.score === 'number' ? c.score : undefined, feedback: c.feedback || undefined }))
  const scores = criteria.flatMap(c => (c.score != null ? [c.score] : []))
  const floor = scores.length > 0 ? Math.min(...scores) : undefined
  return {
    round: typeof b.round === 'number' ? b.round : fallbackRound,
    passed,
    score: typeof b.score === 'number' ? b.score : undefined,
    criteria: criteria.map(c => ({ ...c, failed: !passed && floor != null && c.score === floor })),
  }
}
