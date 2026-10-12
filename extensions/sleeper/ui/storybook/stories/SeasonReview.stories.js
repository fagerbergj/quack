import { page, mobileFrame } from './helpers.js'
import { renderReview } from '../../static/render.js'
import history from '../../fixtures/history.json'

const envelope = { job: 'history', title: '2025 season review', agent: 'history-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: history }

export default { title: 'Cards/SeasonReview' }

export const Default = { render: () => page(renderReview(envelope), '52rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderReview(envelope)) }
