import { page, mobileFrame } from './helpers.js'
import { renderTrade } from '../../static/render.js'
import trade from '../../fixtures/trade.json'
import tradeFinder from '../../fixtures/trade-finder.json'

const envelope = { job: 'trade', title: 'Trade talks', agent: 'trade-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo' }
const talks = [
  { partner: trade.partner, partner_id: trade.partner_id, found: true, status: trade.status, data: trade },
  { partner: 'Jockstrappers', partner_id: '859928030787866624', found: false, status: 'declined', data: null },
]
const partners = [
  { id: trade.partner_id, name: 'Rice Cooker' },
  { id: '859928030787866624', name: 'Jockstrappers' },
  { id: '860265672989609984', name: 'Pitts and Giggles' },
  { id: '860209887672635392', name: 'Achane Magic' },
]
const finder = { found: true, running: false, data: tradeFinder, runnable: true }

export default { title: 'Cards/TradeTalks' }

export const Default = { render: () => page(renderTrade(envelope, talks, 0, partners, finder), '52rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderTrade(envelope, talks, 0, partners, finder)) }
export const NoTalksYet = { render: () => page(renderTrade({ ...envelope, found: false }, [], 0, partners, finder), '52rem') }
// The finder suggestions state: dispatched on demand from the trade card,
// its own artifact kind so it never overwrites a talk.
export const TradeFinderRunning = { render: () => page(renderTrade({ ...envelope, found: false }, [], 0, partners, { found: false, running: true, runnable: true }), '52rem') }

// Backward compatibility: suggestions written before partner_need existed -
// the row renders without the need chip.
const legacyFinder = { ...finder, data: { ...tradeFinder, suggestions: tradeFinder.suggestions.map(({ partner_need, ...rest }) => rest) } }
export const Legacy = { render: () => page(renderTrade({ ...envelope, found: false }, [], 0, partners, legacyFinder), '52rem') }
