import { sidebar, mobileFrame } from './helpers.js'
import { renderDraftSide, renderSeasonAtGlance, renderCrossSeason } from '../../static/render.js'
import draft from '../../fixtures/draft.json'
import history from '../../fixtures/history.json'

const currentEnvelope = { job: 'draft', title: 'Draft analysis', agent: 'draft-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: draft }
const html = () => renderDraftSide(currentEnvelope)
// A past season's draft stop has no clock/plan card - the sidebar falls
// back to season-at-a-glance + cross-season (see main.js's renderMain).
const pastHtml = () => renderSeasonAtGlance(history) + renderCrossSeason(history)

export default { title: 'Cards/DraftSide' }

export const Default = { render: () => sidebar(html()) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(html()) }
export const PastSeasonFallback = { render: () => sidebar(pastHtml()) }
