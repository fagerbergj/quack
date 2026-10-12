import { page, mobileFrame } from './helpers.js'
import { renderLineup } from '../../static/render.js'

// An agent wrote markdown instead of JSON for the lineup job: the card shows a notice and the raw text,
// not a crash.
const rawText = '# Start / sit\n\nStart Josh Allen at QB, he has a great matchup this week.\n\nSit your bench RBs.\n'
const envelope = { job: 'lineup', title: 'Start / sit', agent: 'lineup-analyst', found: true, invalid: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', text: rawText }

export default { title: 'Cards/InvalidState' }

export const Default = { render: () => page(renderLineup(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderLineup(envelope)) }
