// Browser glue for the served page: fetches /sleeper/api/*, wires navigation and Run buttons, and hands
// data to render.js. No build step: native ESM only.
import * as R from './render.js'

R.installAvatarFallback()

const JOBS = [
  { id: 'lineup', name: 'Start / sit', agent: 'lineup-analyst' },
  { id: 'waivers', name: 'Waiver targets', agent: 'waiver-scout' },
  { id: 'trade', name: 'Trade talks', agent: 'trade-analyst' },
  { id: 'digest', name: 'Preview / recap', agent: 'league-reporter' },
  { id: 'trends', name: 'Trends and news', agent: 'trend-scout' },
  { id: 'retro', name: 'Hindsight', agent: 'league-reporter', past: true },
]
const DRAFT_JOB = { id: 'draft', name: 'Draft analysis', agent: 'draft-analyst' }
const REVIEW_JOB = { id: 'history', name: 'Season review', agent: 'history-analyst' }

const qs = new URLSearchParams(location.search)
const state = {
  leagueID: qs.get('league_id') || '',
  seasons: null,   // /api/seasons response
  season: null,    // /api/season response for the selected season
  artifacts: null, // /api/artifacts response for the selected stop
  seasonKey: null, // selected season string
  stop: qs.get('stop') || null,
  talkIdx: 0,
  running: new Set(),      // job ids the server currently reports running
  runnableJobs: new Set(), // job ids with an agent bound (GET /api/jobs)
  // "<season>:<stop>" -> artifact count, filled in as stops are visited (no
  // bulk endpoint exists to precompute every stop's dots up front).
  stopFoundCounts: {},
}

const $ = id => document.getElementById(id)

async function jget(url) {
  const r = await fetch(url)
  if (!r.ok) throw new Error(`${r.status} ${await r.text().catch(() => '')}`)
  return r.json()
}

function banner(msg) {
  $('banner').innerHTML = msg ? `<div class="sl-sec" style="padding:.75rem 1rem;margin-bottom:1rem;color:var(--qk-err-text)">${R.esc(msg)}</div>` : ''
}

async function loadSeasons() {
  const url = state.leagueID ? `api/seasons?league_id=${encodeURIComponent(state.leagueID)}` : 'api/seasons'
  state.seasons = await jget(url)
  state.seasonKey = state.seasons.current_season
  const cur = state.seasons.seasons.find(s => s.season === state.seasonKey)
  state.leagueID = cur.league_id
}

function leagueIDFor(seasonKey) {
  return state.seasons.seasons.find(s => s.season === seasonKey)?.league_id
}

async function loadSeason() {
  state.season = await jget(`api/season?league_id=${encodeURIComponent(state.leagueID)}`)
  if (!state.stop) state.stop = state.seasonKey === state.seasons.current_season ? String(state.season.league.week_now) : 'review'
}

async function loadRunnableJobs() {
  const resp = await jget('api/jobs')
  state.runnableJobs = new Set(resp.runnable || [])
}

// syncRunningFromServer resumes the Running badge and polling for jobs the server reports running.
function syncRunningFromServer() {
  for (const [job, env] of Object.entries(state.artifacts.jobs || {})) {
    if (env.running && !state.running.has(job)) { state.running.add(job); pollFor(job) }
  }
  if ((state.artifacts.talks || []).some(t => t.running) && !state.running.has('trade')) {
    state.running.add('trade')
    pollFor('trade')
  }
  if (state.artifacts.season_notes?.running && !state.running.has('season-notes')) {
    state.running.add('season-notes')
    pollFor('season-notes')
  }
}

async function loadArtifacts() {
  state.artifacts = await jget(`api/artifacts?league_id=${encodeURIComponent(state.leagueID)}&stop=${encodeURIComponent(state.stop)}`)
  state.talkIdx = Math.min(state.talkIdx, Math.max(0, (state.artifacts.talks || []).length - 1))
  const found = Object.values(state.artifacts.jobs || {}).filter(j => j.found).length
  state.stopFoundCounts[`${state.seasonKey}:${state.stop}`] = found
  syncRunningFromServer()
}

function isCurrentSeason() { return state.seasonKey === state.seasons.current_season }
function weekNow() { return state.season.league.week_now }
function isPastWeek() { return state.stop !== 'draft' && state.stop !== 'review' && (!isCurrentSeason() || Number(state.stop) < weekNow()) }

function jobEnvelope(job, title) {
  const a = (state.artifacts.jobs || {})[job.id]
  const found = !!a?.found
  const what = state.stop === 'draft' ? `the ${state.seasonKey} draft` : state.stop === 'review' ? `${state.seasonKey}` : `week ${state.stop}`
  return {
    job: job.id, title: title || job.name, agent: job.agent, what,
    running: state.running.has(job.id), found, runnable: state.runnableJobs.has(job.id),
    status: state.running.has(job.id) ? 'running' : found ? 'done' : 'not_run',
    chatHref: found ? `/chat/ext:sleeper:${state.leagueID}:${state.stop}:${job.id}` : undefined,
    data: a?.data,
    invalid: !!a?.invalid,
    text: a?.text,
  }
}

function renderHeader() {
  const s = state.season
  $('league-name').textContent = s.league.name
  $('league-sub').textContent = isCurrentSeason()
    ? `${s.me?.team ?? ''} · ${s.me ? R.record(s.me) : ''} · ${s.me?.owner ?? ''} · ${s.league.season} season, week ${s.league.week_now} in progress`
    : `${s.me?.team ?? ''} · ${s.me ? R.record(s.me) : ''} · ${s.league.season} season, complete`
  $('league-facts').innerHTML = (s.facts || []).map(f => `<span class="qk-chip">${R.esc(f)}</span>`).join('')
}

function renderYears() {
  $('years').innerHTML = R.renderYears(state.seasons.seasons, state.seasonKey)
}

function renderRail() {
  const prefix = `${state.seasonKey}:`
  const counts = {}
  for (const [key, n] of Object.entries(state.stopFoundCounts)) {
    if (key.startsWith(prefix)) counts[key.slice(prefix.length)] = n
  }
  $('rail').innerHTML = R.renderTimeline(state.stop, weekNow(), isCurrentSeason(), counts)
}

// notRunnableTitle is the disabled-button tooltip for a job with no agent
// bound yet (#C) - undefined lets renderMenuList skip the attribute.
function notRunnableTitle(job) { return state.runnableJobs.has(job) ? undefined : 'no agent bound yet' }

function renderMenu() {
  const list = $('menu-list')
  if (state.stop === 'draft') {
    const label = isCurrentSeason() ? 'Re-grade this draft' : 'Re-run draft report card'
    list.innerHTML = R.renderMenuList(`${state.seasonKey} draft`, [{ job: 'draft', label, agent: DRAFT_JOB.agent, disabled: !state.runnableJobs.has('draft'), title: notRunnableTitle('draft') }])
    return
  }
  if (state.stop === 'review') {
    const label = isCurrentSeason() ? 'Available after week 17' : 'Re-run season review'
    list.innerHTML = R.renderMenuList(`${state.seasonKey} review`, [{ job: 'history', label, agent: REVIEW_JOB.agent, disabled: isCurrentSeason() || !state.runnableJobs.has('history'), title: notRunnableTitle('history') }])
    return
  }
  const past = isPastWeek()
  // Trade is excluded: it targets one specific counterparty chat, and only
  // the trade card itself (its partner select) knows which one to dispatch.
  const jobs = JOBS.filter(j => j.id !== 'trade' && (past ? (j.id === 'retro' || j.id === 'digest') : !j.past))
  const items = jobs.map(j => {
    const env = jobEnvelope(j)
    const label = (env.running ? 'Running ' : env.found ? 'Re-run ' : 'Run ') + j.name.toLowerCase()
    return { job: j.id, label, agent: j.agent, running: env.running, disabled: !env.runnable, title: notRunnableTitle(j.id) }
  })
  list.innerHTML = R.renderMenuList(`${state.seasonKey} · week ${state.stop}`, items)
}

function renderMain() {
  const main = $('main'), side = $('side')
  if (state.stop === 'draft') {
    const env = jobEnvelope(DRAFT_JOB)
    main.innerHTML = R.renderDraftBoard(env)
    if (isCurrentSeason()) {
      side.innerHTML = R.renderDraftSide(env)
    } else {
      side.innerHTML = ''
      loadReviewHistory().then(h => { side.innerHTML = R.renderSeasonAtGlance(h) + R.renderCrossSeason(h) })
    }
    return
  }
  if (state.stop === 'review') {
    const env = jobEnvelope(REVIEW_JOB)
    main.innerHTML = R.renderReview(isCurrentSeason() ? { ...env, found: false } : env)
    side.innerHTML = isCurrentSeason() ? currentSide() : R.renderSeasonAtGlance(env.data) + R.renderCrossSeason(env.data)
    return
  }
  if (isPastWeek()) {
    main.innerHTML = R.renderRetro(jobEnvelope(JOBS[5], `Week ${state.stop} in hindsight`)) + R.renderDigest(jobEnvelope(JOBS[3], `Week ${state.stop} recap`), state.season.me?.team) + (isCurrentSeason() ? R.renderTrends(jobEnvelope(JOBS[4])) : '')
    if (isCurrentSeason()) {
      side.innerHTML = currentSide()
    } else {
      side.innerHTML = ''
      loadReviewHistory().then(h => { side.innerHTML = R.renderSeasonAtGlance(h) + R.renderCrossSeason(h) })
    }
    return
  }
  const talks = state.artifacts.talks || []
  const partners = (state.season.standings || []).filter(t => !t.mine).map(t => ({ id: t.id, name: t.team }))
  const finder = { ...(state.artifacts.jobs || {})['trade-finder'], running: state.running.has('trade-finder'), runnable: state.runnableJobs.has('trade-finder') }
  main.innerHTML = R.renderLineup(jobEnvelope(JOBS[0], 'Start / sit')) + R.renderWaivers(jobEnvelope(JOBS[1], 'Waivers')) +
    R.renderTrade(jobEnvelope(JOBS[2], 'Trade talks'), talks, state.talkIdx, partners, finder) +
    R.renderDigest(jobEnvelope(JOBS[3], `Week ${state.stop} preview`), state.season.me?.team) + R.renderTrends(jobEnvelope(JOBS[4], 'Trends and news'))
  side.innerHTML = currentSide()
}

const historyCache = new Map()
async function loadReviewHistory() {
  if (historyCache.has(state.leagueID)) return historyCache.get(state.leagueID)
  const resp = await jget(`api/artifacts?league_id=${encodeURIComponent(state.leagueID)}&stop=review`)
  const data = resp.jobs?.history?.data ?? null
  historyCache.set(state.leagueID, data)
  return data
}

function currentSide() {
  const notes = state.artifacts.season_notes
  const found = !!notes?.found
  const running = state.running.has('season-notes')
  const notesEnv = { title: 'Season notes', agent: 'trend-scout', found, data: notes?.data, invalid: !!notes?.invalid, text: notes?.text, running, status: running ? 'running' : found ? 'done' : 'not_run', chatHref: found ? `/chat/ext:sleeper:${state.leagueID}:season-notes` : undefined }
  return R.renderStandingsSide(state.season) + R.renderSeasonNotes(notesEnv) + R.renderMovesSide(state.season)
}

function renderAll() {
  renderHeader(); renderYears(); renderRail(); renderMenu(); renderMain()
  const url = new URL(location.href)
  url.searchParams.set('league_id', state.leagueID)
  url.searchParams.set('stop', state.stop)
  history.replaceState(null, '', url)
}

async function refresh() {
  try {
    banner('')
    await loadSeason()
    await loadArtifacts()
    renderAll()
  } catch (err) {
    banner(`Could not load this league: ${err.message}`)
  }
}

async function switchSeason(season) {
  state.seasonKey = season
  state.leagueID = leagueIDFor(season)
  state.stop = season === state.seasons.current_season ? null : 'review'
  state.season = null
  await refresh()
}

async function switchStop(stop) {
  state.stop = stop
  state.talkIdx = 0
  try {
    await loadArtifacts()
    renderAll()
    window.scrollTo({ top: 0 })
  } catch (err) {
    banner(`Could not load this stop: ${err.message}`)
  }
}

function selectedValues(id) {
  const el = document.getElementById(id)
  return el ? Array.from(el.selectedOptions).map(o => o.value) : []
}

// The talkbar's own <select id="talk-partner">: value is the partner's
// stable id, the selected option's text is the display name for the title.
function selectedTalkPartner() {
  const el = document.getElementById('talk-partner')
  if (!el || !el.value) return null
  return { id: el.value, name: el.selectedOptions[0]?.textContent || el.value }
}

async function runJob(job, partner, partnerName) {
  $('menu').removeAttribute('open')
  const args = {}
  if (job === 'trade') {
    // "New talk"/the suggestion CTAs carry no data-partner; fall back to the talkbar's own select.
    if (!partner) {
      const sel = selectedTalkPartner()
      if (sel) { partner = sel.id; partnerName = sel.name }
    }
    if (!partner) { banner('Pick a team to trade with.'); return }
    args.partner = partner
    if (partnerName) args.partner_name = partnerName
    // The compose form's two multi-selects (mine/partner's roster), when present
    // for the talk being acted on - so the analyst gets the actual offer, not just who.
    const give = selectedValues('give')
    const get = selectedValues('get')
    if (give.length) args.give = give.join(',')
    if (get.length) args.get = get.join(',')
  } else if (partner) {
    args.partner = partner
  }
  const body = { league_id: state.leagueID, stop: state.stop, job, args: Object.keys(args).length ? args : undefined }
  state.running.add(job)
  renderMenu(); renderMain()
  try {
    const resp = await fetch('api/jobs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
    if (!resp.ok) throw new Error(await resp.text())
  } catch (err) {
    state.running.delete(job)
    banner(`Could not start the run: ${err.message}`)
    renderMenu(); renderMain()
    return
  }
  pollFor(job)
}

// True while the compose form or any open <details> holds state a blind renderMain() would wipe, so a
// poll tick can't reset an in-progress give/get selection.
function mainHasOpenState() {
  const give = document.getElementById('give'), get = document.getElementById('get')
  if (give?.selectedOptions.length || get?.selectedOptions.length) return true
  return !!document.getElementById('main')?.querySelector('details[open]')
}

// pollTimers dedupes: syncRunningFromServer can call pollFor for a job a
// click already started polling (or vice versa on the next load tick).
const pollTimers = new Map()

// Dispatch is async; poll until the server-side running flag clears (not
// "found" - that fires before the run actually ends) or attempts run out.
function pollFor(job) {
  if (pollTimers.has(job)) return
  let attempts = 0
  const timer = setInterval(async () => {
    attempts++
    try { await loadArtifacts() } catch { /* keep polling; a transient fetch error isn't fatal */ }
    const running = job === 'trade'
      ? (state.artifacts.talks || []).some(t => t.running)
      : job === 'season-notes'
        ? !!state.artifacts.season_notes?.running
        : !!(state.artifacts.jobs || {})[job]?.running
    const done = !running || attempts >= 20
    if (done) {
      clearInterval(timer)
      pollTimers.delete(job)
      state.running.delete(job)
    }
    renderMenu()
    // On the final tick, show the finished card even with an open compose
    // form/details - the guard exists only to protect an in-progress poll.
    if (done || !mainHasOpenState()) renderMain()
  }, 3000)
  pollTimers.set(job, timer)
}

document.addEventListener('click', e => {
  const yb = e.target.closest('[data-season]')
  if (yb) { switchSeason(yb.dataset.season); return }
  const st = e.target.closest('[data-stop]')
  if (st) { switchStop(st.dataset.stop); return }
  const rb = e.target.closest('[data-run]')
  if (rb) { runJob(rb.dataset.run, rb.dataset.partner, rb.dataset.partnerName); return }
  if (!e.target.closest('#menu')) $('menu').removeAttribute('open')
})
document.addEventListener('change', e => {
  if (e.target.id === 'talk-select') { state.talkIdx = Number(e.target.value); renderMain() }
})

async function boot() {
  try {
    await loadRunnableJobs()
    await loadSeasons()
    await refresh()
  } catch (err) {
    banner(`Could not load Sleeper: ${err.message}`)
  }
}
boot()
