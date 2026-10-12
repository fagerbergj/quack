import { page, mobileFrame } from './helpers.js'
import { emptyState } from '../../static/render.js'

// #C: a job with no agent bound yet (e.g. digest) shows a disabled Run
// action with a tooltip instead of dispatching into the planner's guesswork.
const html = emptyState('digest', 'Week 2 preview', 'league-reporter', 'week 2', false)

export default { title: 'Cards/DisabledRun' }

export const Default = { render: () => page(html) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(html) }
