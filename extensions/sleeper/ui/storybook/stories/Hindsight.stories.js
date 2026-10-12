import { page, mobileFrame } from './helpers.js'
import { renderRetro } from '../../static/render.js'
import retro from '../../fixtures/retro.json'

const envelope = { job: 'retro', title: 'Week 1 in hindsight', agent: 'league-reporter', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: retro }

export default { title: 'Cards/Hindsight' }

export const Default = { render: () => page(renderRetro(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderRetro(envelope)) }
