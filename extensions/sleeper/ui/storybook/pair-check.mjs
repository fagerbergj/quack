// Opponent starters come back unnumbered (RB, RB); every numbered slot must still get its opponent.
import { renderLineup } from '../static/render.js'
import lineup from '../fixtures/lineup.json' with { type: 'json' }

const unnumbered = s => s.replace(/\d+$/, '')
const data = { ...lineup, opponent_starters: lineup.opponent_starters.map(o => ({ ...o, slot: unnumbered(o.slot) })) }
const html = renderLineup({ job: 'lineup', title: 'Start / sit', agent: 'x', found: true, status: 'done', chatHref: '#', data })
const cells = [...html.matchAll(/class="sl-opp num">([^<]*)<b>/g)].map(m => m[1])
for (const name of ['B. Robinson', 'K. Walker', 'P. Nacua', 'G. Wilson']) {
  if (!cells.includes(name)) {
    console.error(`FAIL: ${name} missing from opponent cells: ${JSON.stringify(cells)}`)
    process.exit(1)
  }
}
console.log('pair-check: OK')
