// A suggested trade's Start talk must carry that suggestion's give/get, or the talk models a counter from rosters.
import { renderTrade } from '../static/render.js'
import finder from '../fixtures/trade-finder.json' with { type: 'json' }

const html = renderTrade({ job: 'trade', title: 'Trade talks', found: false }, [], 0, [], { found: true, data: finder })
const btns = [...html.matchAll(/<button[^>]*data-run="trade"[^>]*data-partner="([^"]*)"[^>]*>/g)].filter(m => m[1])
const want = finder.suggestions.map(s => [s.give.id, s.get.id])
for (const [give, get] of want) {
  if (!btns.some(m => m[0].includes(`data-give="${give}"`) && m[0].includes(`data-get="${get}"`))) {
    console.error(`FAIL: no Start talk carries give=${give} get=${get}: ${btns.map(m => m[0]).join('\n')}`)
    process.exit(1)
  }
}
console.log(`talk-check: OK (${want.length} suggestions carry their terms)`)
