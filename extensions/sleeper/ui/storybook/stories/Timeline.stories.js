import { page, mobileFrame } from './helpers.js'
import { renderTimeline, renderYears } from '../../static/render.js'
import seasons from '../../fixtures/seasons.json'

function full() {
  return renderYears(seasons.seasons, seasons.current_season) + renderTimeline('2', 2, true, { 2: 4 })
}

export default { title: 'Cards/Timeline' }

export const Default = { render: () => page(full(), '64rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(full()) }
