import { page, mobileFrame } from './helpers.js'
import { renderTrends } from '../../static/render.js'
import trends from '../../fixtures/trends.json'

const envelope = { job: 'trends', title: 'Trends and news', agent: 'trend-scout', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: trends }

export default { title: 'Cards/Trends' }

export const Default = { render: () => page(renderTrends(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderTrends(envelope)) }
