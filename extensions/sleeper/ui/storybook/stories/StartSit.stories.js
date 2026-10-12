import { page, mobileFrame } from './helpers.js'
import { renderLineup } from '../../static/render.js'
import lineup from '../../fixtures/lineup.json'

const envelope = { job: 'lineup', title: 'Start / sit', agent: 'lineup-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: lineup }

export default { title: 'Cards/StartSit' }

export const Default = { render: () => page(renderLineup(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderLineup(envelope)) }

// An artifact without floor/ceiling, replaces, plan or watch must still render, minus those extras.
const stripNew = row => { const { floor, ceiling, replaces, ...rest } = row; return rest }
const legacyData = {
  ...lineup,
  plan: undefined,
  watch: undefined,
  starters: lineup.starters.map(stripNew),
  bench: (lineup.bench || []).map(stripNew),
  opponent_starters: (lineup.opponent_starters || []).map(stripNew),
}
const legacyEnvelope = { ...envelope, data: legacyData }
export const Legacy = { render: () => page(renderLineup(legacyEnvelope)) }

// sleeper_matchup returns opponent slots unnumbered (RB, RB...); a plain {slot: player} map would drop
// the second.
const unnumberedSlot = s => s.replace(/\d+$/, '')
const realOpponentData = { ...lineup, opponent_starters: (lineup.opponent_starters || []).map(o => ({ ...o, slot: unnumberedSlot(o.slot) })) }
const realOpponentEnvelope = { ...envelope, data: realOpponentData }
export const RealOpponentSlots = { render: () => page(renderLineup(realOpponentEnvelope)) }
