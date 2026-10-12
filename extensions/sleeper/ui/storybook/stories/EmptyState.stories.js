import { page, mobileFrame } from './helpers.js'
import { emptyState } from '../../static/render.js'

const html = emptyState('lineup', 'Start / sit', 'lineup-analyst', 'week 2')

export default { title: 'Cards/EmptyState' }

export const Default = { render: () => page(html) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(html) }
