import { page, mobileFrame } from './helpers.js'
import { renderDigest } from '../../static/render.js'
import digest from '../../fixtures/digest.json'

const MY_TEAM = 'Substation Supremacy'
const envelope = { job: 'digest', title: 'Week 2 preview', agent: 'league-reporter', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: digest }

export default { title: 'Cards/Digest' }

export const Default = { render: () => page(renderDigest(envelope, MY_TEAM), '52rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderDigest(envelope, MY_TEAM)) }

// Every game's `mine` flag is deliberately wrong or absent; the page must still highlight Substation
// Supremacy's game by matching /api/season's team name.
const wrongMineData = { ...digest, games: digest.games.map(g => ({ ...g, mine: g.home !== MY_TEAM && g.away !== MY_TEAM })) }
export const IgnoresArtifactMineFlag = { render: () => page(renderDigest({ ...envelope, data: wrongMineData }, MY_TEAM), '52rem') }
