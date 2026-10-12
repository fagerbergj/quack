import { page, mobileFrame } from './helpers.js'
import { kebabMenuHTML } from '../../static/render.js'

const items = [
  { job: 'lineup', label: 'Re-run start / sit', agent: 'lineup-analyst' },
  { job: 'waivers', label: 'Run waiver targets', agent: 'waiver-scout' },
  { job: 'trade', label: 'Run trade talks', agent: 'trade-analyst' },
  { job: 'digest', label: 'Run week 2 preview', agent: 'league-reporter' },
  { job: 'trends', label: 'Running trends and news', agent: 'trend-scout', running: true },
]
const html = kebabMenuHTML('2026 · week 2', items)

export default { title: 'Chrome/KebabMenu' }

export const Default = { render: () => page(html, '20rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(html) }
