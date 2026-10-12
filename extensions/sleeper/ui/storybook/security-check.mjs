// Renders every card with a hostile payload in each numeric-typed field (artifacts are never
// schema-validated on serve) and asserts it never survives unescaped. Node-only: render.js is pure.
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import * as R from '../static/render.js'

const FIXTURES_DIR = path.join(import.meta.dirname, '..', 'fixtures')
const PAYLOAD = '<img src=x onerror=alert(1)>'

async function loadFixture(name) {
  return JSON.parse(await readFile(path.join(FIXTURES_DIR, name + '.json'), 'utf8'))
}

// Deep-clones data, replacing every number leaf (at any depth, in objects
// and arrays alike) with the hostile payload string.
function poisonNumbers(data) {
  if (typeof data === 'number') return PAYLOAD
  if (Array.isArray(data)) return data.map(poisonNumbers)
  if (data && typeof data === 'object') {
    const out = {}
    for (const [k, v] of Object.entries(data)) out[k] = poisonNumbers(v)
    return out
  }
  return data
}

const doneEnvelope = data => ({ job: 'x', title: 'x', agent: 'x', found: true, status: 'done', chatHref: '/chat/x', data })
const invalidEnvelope = text => ({ job: 'x', title: 'x', agent: 'x', found: true, invalid: true, status: 'done', text })

async function checks() {
  const out = []

  // Invalid-artifact raw text lands in a <pre> via esc(); covered via both the job-card and per-talk shapes.
  out.push(['renderLineup (invalid)', R.renderLineup(invalidEnvelope(PAYLOAD))])
  out.push(['renderTrade (invalid)', R.renderTrade(doneEnvelope(null), [{ partner: 'x', partner_id: '1', found: true, invalid: true, text: PAYLOAD }], 0, [{ id: '1', name: 'Team' }])])

  const lineup = poisonNumbers(await loadFixture('lineup'))
  out.push(['renderLineup', R.renderLineup(doneEnvelope(lineup))])

  const waivers = poisonNumbers(await loadFixture('waivers'))
  out.push(['renderWaivers', R.renderWaivers(doneEnvelope(waivers))])

  const trade = poisonNumbers(await loadFixture('trade'))
  const talks = [{ partner: trade.partner, partner_id: String(trade.partner_id), found: true, status: 'open', data: trade }]
  const partners = [{ id: '1', name: 'Team' }]
  out.push(['renderTrade', R.renderTrade(doneEnvelope(null), talks, 0, partners)])

  const tradeFinder = poisonNumbers(await loadFixture('trade-finder'))
  out.push(['renderTrade (finder)', R.renderTrade(doneEnvelope(null), talks, 0, partners, doneEnvelope(tradeFinder))])

  // mdText() parses [text](url) out of agent prose; each hostile link must become a safe <a> or escaped
  // text, never a live javascript: href or raw tag.
  const mdCases = [
    '[click me](javascript:alert(1))',
    '[<img src=x onerror=alert(1)>](https://example.com)',
    '[text](https://example.com/"><script>alert(1)</script>)',
  ]
  const cleanLineup = await loadFixture('lineup')
  for (const payload of mdCases) {
    const poisoned = { ...cleanLineup, starters: cleanLineup.starters.map((s, i) => i === 0 ? { ...s, why: payload } : s) }
    out.push([`renderLineup (md link: ${payload.slice(0, 24)}…)`, R.renderLineup(doneEnvelope(poisoned))])
  }

  const digest = poisonNumbers(await loadFixture('digest'))
  out.push(['renderDigest', R.renderDigest(doneEnvelope(digest), 'Substation Supremacy')])

  const trends = poisonNumbers(await loadFixture('trends'))
  out.push(['renderTrends', R.renderTrends(doneEnvelope(trends))])

  const retro = poisonNumbers(await loadFixture('retro'))
  out.push(['renderRetro', R.renderRetro(doneEnvelope(retro))])

  const draft = poisonNumbers(await loadFixture('draft'))
  out.push(['renderDraftBoard', R.renderDraftBoard(doneEnvelope(draft))])
  out.push(['renderDraftSide', R.renderDraftSide(doneEnvelope(draft))])

  const history = poisonNumbers(await loadFixture('history'))
  const draftReportCard = { ...draft, report_card: history.report_card, pos_alloc: history.pos_alloc }
  out.push(['renderDraftReportCard', R.renderDraftBoard(doneEnvelope(draftReportCard))])
  out.push(['renderReview', R.renderReview(doneEnvelope(history))])
  out.push(['renderSeasonAtGlance', R.renderSeasonAtGlance(history)])
  out.push(['renderCrossSeason', R.renderCrossSeason(history)])

  const notes = poisonNumbers(await loadFixture('season-notes'))
  out.push(['renderSeasonNotes', R.renderSeasonNotes(doneEnvelope(notes))])

  const season = poisonNumbers(await loadFixture('season'))
  out.push(['renderStandingsSide', R.renderStandingsSide(season)])
  out.push(['renderMovesSide', R.renderMovesSide(season)])

  const seasons = poisonNumbers(await loadFixture('seasons'))
  out.push(['renderYears', R.renderYears(seasons.seasons, seasons.current_season)])
  out.push(['renderTimeline', R.renderTimeline('2', 2, true, { 2: 3 })])

  return out
}

async function main() {
  const results = await checks()
  let failed = 0
  for (const [name, html] of results) {
    if (html.includes(PAYLOAD)) {
      failed++
      console.error(`FAIL ${name}: hostile payload survived unescaped into the output`)
    }
    if (/<img\s[^>]*onerror=/i.test(html)) {
      failed++
      console.error(`FAIL ${name}: an unescaped <img onerror=...> tag reached the output`)
    }
    if (/href\s*=\s*["']?\s*javascript:/i.test(html)) {
      failed++
      console.error(`FAIL ${name}: a javascript: URL reached a live href`)
    }
    if (/<script[\s>]/i.test(html)) {
      failed++
      console.error(`FAIL ${name}: an unescaped <script> tag reached the output`)
    }
  }
  console.log(`security-check: ${results.length} renders checked, ${failed} failure(s)`)
  if (failed > 0) process.exit(1)
}

main().catch(err => { console.error(err); process.exit(1) })
