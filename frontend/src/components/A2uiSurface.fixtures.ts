import raw from './A2uiSurface.fixtures.json'
import type { SurfaceContent } from '../lib/a2ui'

// Real qwen3.8-27b render_ui output for two PRs (a2ui spike), plus the grading
// upsert contract section 4 describes, built from each surface's answer key.
type Fixture = (typeof raw)['pr1085']

function build(f: Fixture) {
  const first = f.surface as SurfaceContent
  const byId = new Map(first.components.map(c => [c.id, c]))
  for (const c of f.grade) byId.set(c.id, c)
  const graded: SurfaceContent = { ...first, components: [...byId.values()], data_model: { answers: f.picks } }
  return { first, graded, picks: f.picks }
}

export const pr1085 = build(raw.pr1085)
export const pr9 = build(raw.pr9 as Fixture)
