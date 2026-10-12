// Composes the whole served page from static fixtures (no fetch/main.js) -
// what a viewer sees for a populated current week.
import * as R from '../../static/render.js'
import season from '../../fixtures/season.json'
import seasons from '../../fixtures/seasons.json'
import lineup from '../../fixtures/lineup.json'
import waivers from '../../fixtures/waivers.json'
import trade from '../../fixtures/trade.json'
import tradeFinder from '../../fixtures/trade-finder.json'
import digest from '../../fixtures/digest.json'
import trends from '../../fixtures/trends.json'
import notes from '../../fixtures/season-notes.json'

const done = (job, title, agent, data) => ({ job, title, agent, found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data })

function fullPage() {
  const header = `<header class="qk-page__header"><div><h1>${R.esc(season.league.name)}</h1><p>${R.esc(season.me.team)} · ${season.me.wins}-${season.me.losses} · ${season.league.season} season, week ${season.league.week_now} in progress</p><div class="sl-facts">${season.facts.map(f => `<span class="qk-chip">${R.esc(f)}</span>`).join('')}</div></div></header>`
  const talks = [{ partner: trade.partner, partner_id: trade.partner_id, found: true, status: trade.status, data: trade }]
  const partners = (season.standings || []).filter(t => !t.mine).map(t => ({ id: t.id, name: t.team }))
  const finder = { found: true, running: false, data: tradeFinder, runnable: true }
  const main = R.renderLineup(done('lineup', 'Start / sit', 'lineup-analyst', lineup)) +
    R.renderWaivers(done('waivers', 'Waivers', 'waiver-scout', waivers)) +
    R.renderTrade(done('trade', 'Trade talks', 'trade-analyst'), talks, 0, partners, finder) +
    R.renderDigest(done('digest', 'Week 2 preview', 'league-reporter', digest), season.me.team) +
    R.renderTrends(done('trends', 'Trends and news', 'trend-scout', trends))
  const side = R.renderStandingsSide(season) +
    R.renderSeasonNotes(done('season-notes', 'Season notes', 'trend-scout', notes)) +
    R.renderMovesSide(season)
  return `<div class="qk-page__inner">${header}${R.renderYears(seasons.seasons, seasons.current_season)}${R.renderTimeline('2', 2, true, { 2: 5 })}<div class="sl-grid"><div class="sl-main">${main}</div><aside class="sl-side">${side}</aside></div></div>`
}

export default { title: 'Pages/FullPage' }

export const Default = { render: () => { const el = document.createElement('div'); el.className = 'qk-page'; el.innerHTML = fullPage(); return el } }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = {
  render: () => {
    const el = document.createElement('div')
    el.className = 'qk-page'
    // No overflow-x:hidden - that would clip the very overflow render-check measures.
    el.style.cssText = 'width:390px;max-width:390px;border:1px solid var(--qk-border)'
    el.innerHTML = fullPage()
    return el
  },
}
