import { page, mobileFrame } from './helpers.js'
import { renderWaivers } from '../../static/render.js'
import waivers from '../../fixtures/waivers.json'

const envelope = { job: 'waivers', title: 'Waivers', agent: 'waiver-scout', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: waivers }

export default { title: 'Cards/Waivers' }

export const Default = { render: () => page(renderWaivers(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderWaivers(envelope)) }

// An artifact without waiver_type/my_priority/teams/also_checked still renders its numbered claim rows.
const { waiver_type, my_priority, teams, also_checked, ...legacyData } = waivers
const legacyEnvelope = { ...envelope, data: legacyData }
export const Legacy = { render: () => page(renderWaivers(legacyEnvelope)) }
