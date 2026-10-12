import { page, mobileFrame } from './helpers.js'
import { renderWaivers } from '../../static/render.js'

const envelope = { job: 'waivers', title: 'Waivers', agent: 'waiver-scout', found: false, running: true, status: 'running', what: 'week 2' }
const html = renderWaivers(envelope)

export default { title: 'Cards/Running' }

export const Default = { render: () => page(html) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(html) }
