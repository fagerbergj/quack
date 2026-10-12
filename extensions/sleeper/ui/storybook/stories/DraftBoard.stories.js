import { page, mobileFrame } from './helpers.js'
import { renderDraftBoard } from '../../static/render.js'
import draft from '../../fixtures/draft.json'
import history from '../../fixtures/history.json'

const boardEnvelope = { job: 'draft', title: 'Draft analysis', agent: 'draft-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: draft }
const reportCardEnvelope = { ...boardEnvelope, data: { ...draft, report_card: history.report_card, pos_alloc: history.pos_alloc } }

export default { title: 'Cards/DraftBoard' }

export const Default = { render: () => page(renderDraftBoard(boardEnvelope), '64rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderDraftBoard(boardEnvelope)) }
// A past season shows a report card (drafted-as vs finished) instead of the live board.
export const ReportCard = { render: () => page(renderDraftBoard(reportCardEnvelope), '56rem') }
