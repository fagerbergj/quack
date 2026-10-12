// Pure renderers (string in, HTML string out, no DOM or fetch) shared by main.js and every Storybook
// story, so a card renders identically in both.

export const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
export const num = (n, d = 1) => Number(n ?? 0).toFixed(d)

// Sleeper's ADP sentinel for "no ADP" is 999/1000, not null - a truthy
// value, so callers must check this instead of `p.adp` directly.
export const hasADP = adp => adp != null && adp < 999

// Agent-written links are the only untrusted href source and artifacts are never schema-validated on
// serve, so esc() alone would let a javascript: URL through.
export const isSafeHref = url => /^https?:\/\//i.test(String(url ?? ''))

// Coerces an agent-written "numeric" field to a number (or ''): nothing on the serve path enforces
// the schema's number type.
export const int = n => (n != null && Number.isFinite(Number(n)) ? Number(n) : '')

// Renders markdown [text](url) as a real <a> (http/https only); everything
// else, including a non-http(s) link, is escaped literal text.
const MD_LINK = /\[([^\]]*)\]\(([^)]*)\)/g
export function mdText(s) {
  const str = String(s ?? '')
  let out = '', last = 0, m
  MD_LINK.lastIndex = 0
  while ((m = MD_LINK.exec(str))) {
    out += esc(str.slice(last, m.index))
    out += isSafeHref(m[2]) ? `<a href="${esc(m[2])}" target="_blank" rel="noopener noreferrer">${esc(m[1])}</a>` : esc(m[0])
    last = MD_LINK.lastIndex
  }
  return out + esc(str.slice(last))
}

// A <details> wrapper whose closed state CSS-line-clamps its own <summary>
// to 2 lines (page.css's .sl-clamp) - one content source, no duplicated text.
export function clampBlock(innerHTML) {
  return `<details class="sl-clamp"><summary>${innerHTML}</summary></details>`
}

// DEF names are "<City words> <Nickname> DEF"; the lineup columns need just
// the city, which is every word but the last two.
function teamCity(name) {
  const words = String(name || '').trim().split(/\s+/)
  return words.length >= 3 && words[words.length - 1] === 'DEF' ? words.slice(0, -2).join(' ') : name
}

// "Justin Jefferson" -> "J. Jefferson" - the opponent lineup column has no
// room for full names next to a range bar.
function abbrevName(name) {
  const words = String(name || '').trim().split(/\s+/)
  return words.length < 2 ? name : `${words[0][0]}. ${words.slice(1).join(' ')}`
}

// slotPlayerName picks the display form for one lineup row: DEF is always
// city-only (both sides); a non-DEF opponent is abbreviated to save width.
function slotPlayerName(player, mine) {
  if (!player) return ''
  if (player.pos === 'DEF') return teamCity(player.name)
  return mine ? player.name : abbrevName(player.name)
}

// Standard normal CDF via the Abramowitz-Stegun erf approximation (good to
// ~1e-7) - good enough for an illustrative win chance, no stats dependency.
function erf(x) {
  const s = x < 0 ? -1 : 1
  x = Math.abs(x)
  const a1 = 0.254829592, a2 = -0.284496736, a3 = 1.421413741, a4 = -1.453152027, a5 = 1.061405429, p = 0.3275911
  const t = 1 / (1 + p * x)
  const y = 1 - (((((a5 * t + a4) * t) + a3) * t + a2) * t + a1) * t * Math.exp(-x * x)
  return s * y
}
const normalCdf = x => 0.5 * (1 + erf(x / Math.SQRT2))

// Treats each team's floor/ceiling as its 10th/90th percentile (normal,
// z=1.2816) to get a variance, then P(win) = Phi(diff / sqrt(var1 + var2)).
const Z90 = 1.2815515655446004
function winChance(myProj, oppProj, myFloor, myCeiling, oppFloor, oppCeiling) {
  const v1 = ((myCeiling - myFloor) / (2 * Z90)) ** 2
  const v2 = ((oppCeiling - oppFloor) / (2 * Z90)) ** 2
  const sd = Math.sqrt(v1 + v2)
  return sd > 0 ? normalCdf((myProj - oppProj) / sd) : myProj >= oppProj ? 1 : 0
}

// Sums each player's own variance (same z as winChance), not the raw
// floors/ceilings - that would assume every starter busts or booms together.
function teamRange(starters) {
  if (!starters?.length || starters.some(s => s.floor == null || s.ceiling == null)) return null
  let mean = 0, variance = 0
  for (const s of starters) {
    mean += s.proj
    variance += ((s.ceiling - s.floor) / (2 * Z90)) ** 2
  }
  const spread = Z90 * Math.sqrt(variance)
  return { floor: mean - spread, ceiling: mean + spread }
}

// One shared 0..scale axis for every bar in the card, so rows stay visually
// comparable: the highest ceiling, rounded up to a multiple of 4.
function barScale(starters) {
  const max = Math.max(4, ...starters.map(s => s.ceiling).filter(c => c != null))
  return Math.ceil(max / 4) * 4
}

// Floor-ceiling range bar with the projection tick; an inverted pair (a
// stray agent value) is dropped rather than rendered backwards.
function rangeBar(floor, ceiling, proj, scale) {
  if (floor == null || ceiling == null || ceiling < floor) return ''
  const left = (100 * floor / scale).toFixed(1), width = (100 * (ceiling - floor) / scale).toFixed(1)
  const tick = proj != null ? (100 * proj / scale).toFixed(1) : null
  return `<div class="sl-bar"><span class="sl-range" aria-label="floor ${num(floor, 0)}, ceiling ${num(ceiling, 0)}"><span class="sl-band" style="left:${left}%;width:${width}%"></span>${tick != null ? `<span class="sl-tick" style="left:${tick}%"></span>` : ''}</span><span class="sl-rng num">${num(floor, 0)}–${num(ceiling, 0)}</span></div>`
}

// Ties only when non-zero - mirrors sleeper.recordString (Go) so a team's
// record never disagrees between the header and any per-team row.
export const record = t => (t?.ties ? `${int(t.wins) || 0}-${int(t.losses) || 0}-${int(t.ties) || 0}` : `${int(t?.wins) || 0}-${int(t?.losses) || 0}`)

// A Sleeper player_id is all digits; a DEF's id is its team code (e.g.
// "NE") and has no player thumbnail, only a team logo.
export function avatarUrl(id) {
  if (!id) return null
  return /^\d+$/.test(id)
    ? `https://sleepercdn.com/content/nfl/players/thumb/${id}.jpg`
    : `https://sleepercdn.com/images/team_logos/nfl/${id.toLowerCase()}.png`
}

export function avatarHTML(player, sm) {
  const cls = 'av' + (sm ? ' av--sm' : '')
  const initials = (player?.name || '?').split(' ').map(w => w[0]).slice(0, 2).join('')
  if (!player?.id) return `<span class="${cls} av--i">${esc(initials)}</span>`
  const url = esc(avatarUrl(player.id))
  // No inline onerror (names are agent-written): installAvatarFallback's delegated listener does the swap.
  return `<img class="${cls}" src="${url}" alt="" loading="lazy" data-avatar-fallback="${esc(initials)}" data-avatar-class="${esc(cls)} av--i">`
}

// Installs the delegated image-fallback listener once; main.js and preview.js both call it.
export function installAvatarFallback(root = document) {
  root.addEventListener('error', e => {
    const img = e.target
    if (!(img instanceof HTMLImageElement) || img.dataset.avatarFallback == null) return
    const span = document.createElement('span')
    span.className = img.dataset.avatarClass
    span.textContent = img.dataset.avatarFallback
    img.replaceWith(span)
  }, true)
}

export function injBadge(p) {
  if (!p?.inj) return ''
  const cls = p.inj === 'IR' || p.inj === 'Out' ? 'qk-badge--err' : 'qk-badge--warn'
  return `<span class="qk-badge ${cls}">${esc(p.inj)}</span>`
}

// meta adds optional chips: [rawHTML, ...], decision-context pills (e.g.
// "2 changes") rendered next to the title, apart from the status meta.
export function jobHead(meta) {
  let m
  if (meta.status === 'running') m = `<span class="qk-badge qk-badge--warn">Running</span><span>${esc(meta.agent || '')} · started just now</span>`
  else if (meta.status === 'done') m = `<span class="qk-badge qk-badge--ok">From chat</span><span>${esc(meta.agent || '')}</span><a href="${esc(meta.chatHref || '#')}" target="_top">Open chat</a>`
  else m = '<span class="qk-badge">Not run</span>'
  const chips = (meta.chips || []).join('')
  return `<div class="sl-sec__head"><div class="sl-sec__title"><h2>${esc(meta.title)}</h2>${chips}</div><div class="sl-sec__meta">${m}</div></div>`
}

// emptyNoun is the job-general noun for the "not run yet" copy; jobHead already carries the week/stop.
const emptyNoun = {
  lineup: 'start / sit', waivers: 'waivers', trade: 'trade talks',
  digest: 'preview', trends: 'trends and news', retro: 'hindsight',
  draft: 'draft analysis', history: 'season review',
}

export function emptyBody(job, jobLabel, what, running, runnable = true) {
  if (running) return `<div class="sl-sec__body sl-empty"><span>${esc(jobLabel)} is working on ${esc(what)}. The card fills in when the run finishes.</span></div>`
  const noun = emptyNoun[job] || jobLabel.toLowerCase()
  const attrs = runnable ? '' : ' disabled title="no agent bound yet"'
  return `<div class="sl-sec__body sl-empty"><span>No ${esc(noun)} yet.</span><button class="qk-btn qk-btn--primary" data-run="${esc(job)}"${attrs}>Run ${esc(noun)}</button></div>`
}

export function section(id, inner) {
  return `<section class="sl-sec" id="${esc(id)}">${inner}</section>`
}

// The artifact's bytes weren't JSON (readArtifact's server-side fallback) -
// show the raw text rather than crashing on state.data being absent.
export function invalidBody(text) {
  return `<div class="sl-sec__body sl-invalid"><p class="sl-invalid__notice">Output is not in the expected format</p><pre class="sl-invalid__raw">${esc(text)}</pre></div>`
}

// pairOppBySlot matches opponent_starters' raw, unnumbered slots (RB, RB...)
// to my own numbered slots (RB1, RB2...): the Nth opponent at a base position pairs with the Nth of mine.
function pairOppBySlot(myStarters, oppStarters) {
  const basePos = slot => String(slot || '').replace(/\d+$/, '')
  const queues = {}
  for (const o of oppStarters || []) {
    const pos = basePos(o.slot)
    ;(queues[pos] = queues[pos] || []).push(o)
  }
  const out = {}
  for (const s of myStarters || []) {
    const q = queues[basePos(s.slot)]
    if (q && q.length) out[s.slot] = q.shift()
  }
  return out
}

// One starter row: your player against the opponent's at that slot. A
// `replaces` row is highlighted and always expandable; a plain row only if it has a `why`.
function lineupRow(row, scale, oppBySlot) {
  const changed = !!row.replaces
  const opp = oppBySlot[row.slot]
  const canExpand = changed || row.why
  const meLine = `<div class="sl-line"><span class="sl-who">${esc(slotPlayerName(row.player, true))}</span>${changed ? ' <span class="sl-dir sl-dir--in">IN</span>' : ''}${injBadge(row.player)}<span class="sl-pts num">${num(row.proj)}</span></div>${rangeBar(row.floor, row.ceiling, row.proj, scale)}`
  const oppCell = opp ? `<span class="sl-opp num">${esc(slotPlayerName(opp.player, false))}<b>${num(opp.proj)}</b></span>` : '<span class="sl-opp"></span>'
  const summaryInner = `<span class="sl-slot">${esc(row.slot)}${canExpand ? '<span class="ms sl-xchev" aria-hidden="true">expand_more</span>' : ''}</span><div class="sl-me">${meLine}</div>${oppCell}`
  if (!canExpand) return `<div class="sl-lrow${changed ? ' sl-lrow--changed' : ''}">${summaryInner}</div>`
  const why = changed
    ? `<span class="sl-was">over ${esc(row.replaces.name)} ${injBadge(row.replaces)}.</span> ${mdText(row.why || '')}`
    : mdText(row.why || '')
  return `<details class="sl-xrow"><summary class="sl-lrow${changed ? ' sl-lrow--changed' : ''}">${summaryInner}</summary><p class="sl-why sl-xwhy">${why}</p></details>`
}

// Bench/reserve rows are folded, compact (name, proj, range as text - no bar).
function foldedRow(item) {
  const range = item.floor != null && item.ceiling != null ? ` · ${num(item.floor, 0)}–${num(item.ceiling, 0)}` : ''
  return `<tr title="${esc(item.why || '')}"><td class="cell">${esc(item.player.name)}${injBadge(item.player)}</td><td class="num">${num(item.proj)}${range}</td></tr>`
}

// state: {job, title, agent, status, chatHref, found, data, what, running}
export function renderLineup(state) {
  const clean = state.found && !state.invalid
  const changes = clean ? (state.data.starters || []).filter(s => s.replaces).length : 0
  const chips = []
  if (changes) chips.push(`<span class="qk-chip sl-chip--accent">${changes} change${changes === 1 ? '' : 's'}</span>`)
  if (clean && state.data.plan) chips.push(`<span class="qk-chip">${esc(state.data.plan)}</span>`)
  const head = jobHead({ ...state, chips })
  if (state.invalid) return section('sec-lineup', head + invalidBody(state.text))
  if (!state.found) return section('sec-lineup', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  const starters = d.starters || []
  const scale = barScale(starters)
  const oppBySlot = pairOppBySlot(starters, d.opponent_starters || [])
  const rows = starters.map(row => lineupRow(row, scale, oppBySlot)).join('')
  const bench = d.bench || [], reserve = d.reserve || []

  const myRange = teamRange(starters)
  const oppRange = teamRange(d.opponent_starters || [])
  const hasProj = d.my_proj != null && d.opp_proj != null
  const winPct = myRange && oppRange && hasProj ? Math.round(100 * winChance(d.my_proj, d.opp_proj, myRange.floor, myRange.ceiling, oppRange.floor, oppRange.ceiling)) : null
  const h2h = d.opponent ? `<div class="sl-h2h" aria-label="Week ${int(d.week)} matchup">
    <div class="sl-team"><span class="sl-name">${esc(d.team)}</span><span class="sl-rec">${esc(d.team_record || '')} · you</span>${hasProj ? `<span class="sl-tot num">${num(d.my_proj)}</span>` : ''}${myRange ? `<span class="sl-rng-lbl num">range ${num(myRange.floor, 0)}–${num(myRange.ceiling, 0)}</span>` : ''}</div>
    ${winPct != null ? `<div class="sl-odds"><span class="sl-pct num">${winPct}%</span><span class="sl-lbl">to win</span></div>` : '<div></div>'}
    <div class="sl-team sl-team--r"><span class="sl-name">${esc(d.opponent)}</span><span class="sl-rec">${esc(d.opponent_record || '')}</span>${hasProj ? `<span class="sl-tot num">${num(d.opp_proj)}</span>` : ''}${oppRange ? `<span class="sl-rng-lbl num">range ${num(oppRange.floor, 0)}–${num(oppRange.ceiling, 0)}</span>` : ''}</div>
  </div>` : ''
  const watch = (d.watch || []).map(w => `<div class="sl-watch"><span class="ms" aria-hidden="true">schedule</span><p><b>Watch ${esc(w.player?.name || '')}${injBadge(w.player)}.</b> ${mdText(w.text || '')}</p></div>`).join('')
  return section('sec-lineup', head + h2h + `
    <div class="sl-lineup" aria-label="Your starters against your opponent's">
    <div class="sl-lrow sl-lrow--head"><span>Slot</span><span>You · floor–ceiling</span><span class="sl-r">Them</span></div>
    ${rows}
    </div>
    ${watch}
    <details class="sl-fold"><summary>Bench · ${bench.length}</summary><div class="qk-table-wrap"><table class="qk-table">${bench.map(foldedRow).join('')}</table></div></details>
    ${reserve.length ? `<details class="sl-fold"><summary>Injured reserve · ${reserve.length}</summary><div class="qk-table-wrap"><table class="qk-table">${reserve.map(foldedRow).join('')}</table></div></details>` : ''}
    <div class="sl-sec__body"><p class="sl-src">${mdText(d.source_note || '')}</p></div>`)
}

// "Rolling waivers · you're 6th of 10 · submit in this order" - built from
// the three optional fields rather than stored, so it can't drift from them.
function waiverSub(d) {
  if (!d.waiver_type) return ''
  const label = { rolling: 'Rolling waivers', faab: 'FAAB', 'reverse standings': 'Reverse-standings waivers' }[d.waiver_type.toLowerCase()] || esc(d.waiver_type)
  const pos = d.my_priority != null && d.teams != null ? ` · you're ${int(d.my_priority)}${ordinal(d.my_priority)} of ${int(d.teams)}` : ''
  return `<p class="sl-sub">${label}${pos} · submit in this order</p>`
}
const ordinal = n => (n % 10 === 1 && n % 100 !== 11 ? 'st' : n % 10 === 2 && n % 100 !== 12 ? 'nd' : n % 10 === 3 && n % 100 !== 13 ? 'rd' : 'th')

// How-claims-run copy is picked by waiver_type - each type resolves ties
// and ordering differently, so one blurb would mislead two of the three.
const CLAIM_RULES = {
  rolling: 'Claims process in waiver order, and a successful claim sends you to the back. Two claims can name the same drop; once one succeeds, the other is skipped (<a href="https://support.sleeper.com/en/articles/3978623-why-is-my-waiver-claim-invalid" target="_blank" rel="noopener noreferrer">Sleeper</a>).',
  faab: 'Highest bid wins each player; ties go to whoever is earlier in waiver order. A failed bid does not spend the budget, so a fallback claim on the same drop is safe to submit alongside your top bid.',
  'reverse standings': 'Claims process worst-record-first for the week, then reset next week regardless of who won a claim. Two claims can name the same drop; once one succeeds, the other is skipped.',
}

export function renderWaivers(state) {
  const claims = state.found && !state.invalid ? (state.data.candidates || []).length : 0
  const chips = claims ? [`<span class="qk-chip sl-chip--accent">${claims} claim${claims === 1 ? '' : 's'}</span>`] : []
  const head = jobHead({ ...state, chips })
  if (state.invalid) return section('sec-waivers', head + invalidBody(state.text))
  if (!state.found) return section('sec-waivers', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  const candidates = d.candidates || []
  // Flags a claim whose drop repeats an earlier claim's - it only runs if
  // that earlier claim fails, which the row's meta text should say plainly.
  const seenDrops = new Set()
  const rows = candidates.map((c, i) => {
    const dup = c.drop && seenDrops.has(c.drop)
    if (c.drop) seenDrops.add(c.drop)
    const dropMeta = dup ? `same drop as ${candidates.findIndex(x => x.drop === c.drop) + 1}` : ''
    const addLine = `<div class="sl-line"><span class="sl-dir sl-dir--in">ADD</span><span class="sl-who">${esc(c.player.name)}</span><span class="sl-meta">${esc(c.player.pos || '')}${c.player.team ? ' · ' + esc(c.player.team) : ''}</span>${injBadge(c.player)}<span class="sl-pts num">${num(c.proj)}</span></div>`
    const dropLine = c.drop ? `<div class="sl-line"><span class="sl-dir sl-dir--out">DROP</span><span class="sl-who">${esc(c.drop)}</span>${dropMeta ? `<span class="sl-meta">${dropMeta}</span>` : ''}</div>` : ''
    const body = `<span class="sl-tag num">${int(c.rank) || i + 1}</span><div class="sl-pair">${addLine}${dropLine}</div>`
    if (!c.why) return `<div class="sl-row">${body}</div>`
    return `<details class="sl-xrow"><summary class="sl-row">${body}<span class="ms sl-xchev" aria-hidden="true">expand_more</span></summary><p class="sl-why sl-xwhy">${mdText(c.why)}</p></details>`
  }).join('')
  const alsoChecked = d.also_checked || []
  const alsoBody = alsoChecked.length
    ? `<details class="sl-fold"><summary>Also checked · ${alsoChecked.length}</summary><div class="sl-fold-body"><ul>${alsoChecked.map(a => `<li>${esc(a.player.name)} (${esc(a.player.pos || '')}): ${mdText(a.why)}</li>`).join('')}</ul></div></details>`
    : ''
  const rulesText = d.waiver_type && CLAIM_RULES[d.waiver_type.toLowerCase()]
  const rulesBody = rulesText
    ? `<details class="sl-fold"><summary>How claims run</summary><div class="sl-fold-body"><p>${rulesText}</p></div></details>`
    : ''
  return section('sec-waivers', head + waiverSub(d) + `<div class="sl-rows">${rows}</div>${alsoBody}${rulesBody}
    <div class="sl-sec__body"><p class="sl-src">${mdText(d.source_note || '')}</p></div>`)
}

function tradeSide(label, players) {
  return `<div><div class="lbl">${esc(label)}</div>${(players || []).map(p => `<span class="sl-p">${avatarHTML(p, true)}<span>${esc(p.name)} <span class="sl-keep">${esc(p.pos || '')} · ${num(p.proj)}</span> ${injBadge(p)}</span></span>`).join('')}</div>`
}

function tradeOption(p) {
  return `<option value="${esc(p.id)}">${esc(p.name)} · ${esc(p.pos || '')} · ${num(p.proj)}</option>`
}

// runAttrs disables a Run action with a tooltip when its job has no agent
// bound yet (#C) - reused everywhere a trade button can trigger a dispatch.
function runAttrs(runnable) {
  return runnable ? '' : ' disabled title="no agent bound yet"'
}

// One suggestion row: partner + record/place + partner_need chip, GIVE/GET
// lines with projections, a Start-talk button; note sits behind the expand.
function tradeFinderRow(f, runnable) {
  const startBtn = `<button class="qk-btn" data-run="trade" data-partner="${esc(f.partner_id || f.partner)}" data-partner-name="${esc(f.partner)}"${runAttrs(runnable)}><span class="ms" aria-hidden="true">forum</span>Start talk</button>`
  const partnerLine = `<div class="sl-partner"><b>${esc(f.partner)}</b>${f.record ? `<span class="sl-meta">${esc(f.record)}</span>` : ''}${f.partner_need ? `<span class="qk-chip">${esc(f.partner_need)}</span>` : ''}${startBtn}</div>`
  const giveLine = `<div class="sl-line"><span class="sl-dir sl-dir--out">GIVE</span><span class="sl-who">${esc(f.give.name)}</span><span class="sl-meta">${esc(f.give.pos || '')}</span>${injBadge(f.give)}<span class="sl-pts num sl-keep">${num(f.give_proj)}</span></div>`
  const getLine = `<div class="sl-line"><span class="sl-dir sl-dir--in">GET</span><span class="sl-who">${esc(f.get.name)}</span><span class="sl-meta">${esc(f.get.pos || '')}</span>${injBadge(f.get)}<span class="sl-pts num">${num(f.get_proj)}</span></div>`
  const body = `<div class="sl-pair">${partnerLine}${giveLine}${getLine}</div>`
  if (!f.note) return `<div class="sl-row sl-row--trade">${body}</div>`
  return `<details class="sl-xrow"><summary class="sl-row sl-row--trade">${body}<span class="ms sl-xchev" aria-hidden="true">expand_more</span></summary><p class="sl-why sl-xwhy">${mdText(f.note)}</p></details>`
}

// finder: the trade-finder job's own artifactEnvelope ({found,
// running, data}) - a separate kind so a finder run never overwrites a talk.
function tradeFinder(finder, runnable = true) {
  const suggestions = finder?.data?.suggestions
  const running = !!finder?.running
  const cta = running
    ? '<span class="qk-badge qk-badge--warn">Finding trades…</span>'
    : `<button class="qk-btn" data-run="trade-finder"${runAttrs(runnable)}>Find trades</button>`
  if (!suggestions?.length) {
    const msg = running ? 'Looking for trades that help both sides.' : 'No suggestions yet.'
    return `<h3 class="sl-h3">Suggested trades</h3><div class="sl-empty"><span>${esc(msg)}</span> ${cta}</div>`
  }
  const rows = suggestions.map(f => tradeFinderRow(f, runnable)).join('')
  return `<h3 class="sl-h3">Suggested trades</h3><div class="sl-rows">${rows}</div>${cta}`
}

function talkPartnerSelect(partners) {
  // A <select> always reports its first option selected; the blank lead option lets runJob's
  // "Pick a team to trade with." guard fire.
  const opts = (partners || []).map(p => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join('')
  return `<select id="talk-partner" class="sl-select" aria-label="Team to trade with"><option value="">Pick a team…</option>${opts}</select>`
}

// talks: [{partner, found, status, data}], talkIdx picks the shown one; partners feeds the
// "New talk" select; finder is the trade-finder envelope for the suggestions CTA.
export function renderTrade(state, talks, talkIdx, partners, finder) {
  const partnerCount = finder?.data?.suggestions?.length || 0
  const chips = partnerCount ? [`<span class="qk-chip">${partnerCount} partner${partnerCount === 1 ? '' : 's'}</span>`] : []
  const head = jobHead({ ...state, chips })
  const anyFound = talks.some(t => t.found)
  const finderRunnable = finder?.runnable !== false
  if (!anyFound) {
    // The talkbar (partner select + New talk) is the only way to start a
    // FIRST talk when no suggestions exist yet, so it has to render here too.
    const runnable = state.runnable !== false
    return section('sec-trade', head + `<div class="sl-sec__body">
    <div class="sl-talkbar">${talkPartnerSelect(partners)}<button class="qk-btn qk-btn--primary" data-run="trade"${runAttrs(runnable)}>New talk</button></div>
    ${tradeFinder(finder, finderRunnable)}<div class="sl-empty" style="margin-top:1rem"><span>Pick a team and start a talk.</span></div></div>`)
  }
  const runnable = state.runnable !== false
  const cur = talks[Math.min(talkIdx, talks.length - 1)]
  const options = talks.map((t, i) => `<option value="${i}" ${i === talkIdx ? 'selected' : ''}>${esc(t.partner)} · ${esc(t.status || (t.found ? 'open' : 'new'))} · ${t.data?.offers?.length ?? 0} offer${(t.data?.offers?.length ?? 0) === 1 ? '' : 's'}</option>`).join('')
  const talkbar = `<div class="sl-talkbar"><label for="talk-select" class="sl-keep" style="font-size:.75rem">Talk</label><select id="talk-select" class="sl-select">${options}</select>${talkPartnerSelect(partners)}<button class="qk-btn" data-run="trade"${runAttrs(runnable)}>New talk</button></div>`
  if (cur.invalid) return section('sec-trade', head + `<div class="sl-sec__body">${talkbar}</div>` + invalidBody(cur.text))
  const d = cur.data
  const offers = d?.offers?.length
    ? d.offers.map(o => `<li class="sl-offer ${o.by === 'You' ? 'mine' : ''}"><div class="sl-offer__who"><b>${esc(o.by)}</b>${esc(o.when)}</div><div><div class="sl-sides">${tradeSide('You give', o.give)}${tradeSide('You get', o.get)}</div><p class="sl-eval"><span class="qk-badge ${o.verdict === 'send' ? 'qk-badge--ok' : o.verdict === 'decline' ? 'qk-badge--err' : 'qk-badge--warn'}">${esc(o.verdict)}</span> <b>${esc(o.delta)}</b> · ${mdText(o.why)}</p></div></li>`).join('')
    : `<li class="sl-empty">No offers yet.</li>`
  const compose = d
    ? `<form class="sl-compose" id="compose" data-partner="${esc(cur.partner)}"><div><label for="give">You give</label><select id="give" class="sl-select sl-multi" multiple size="5">${(d.my_roster || []).map(tradeOption).join('')}</select></div><div><label for="get">You get from ${esc(cur.partner)}</label><select id="get" class="sl-select sl-multi" multiple size="5">${(d.partner_roster || []).map(tradeOption).join('')}</select></div><button class="qk-btn qk-btn--primary" data-run="trade" data-partner="${esc(cur.partner_id)}" data-partner-name="${esc(cur.partner)}" type="button"${runAttrs(runnable)}>Evaluate counter</button></form>`
    : ''
  return section('sec-trade', head + `<div class="sl-sec__body">
    ${talkbar}
    <ul class="sl-offers">${offers}</ul>
    ${compose}
    <div style="margin-top:1.25rem;padding-top:1rem;border-top:1px solid var(--qk-border)">${tradeFinder(finder, finderRunnable)}</div>
    <p class="sl-src">One chat per talk; every offer or counter is a new turn, so the analyst keeps the whole negotiation in context.</p></div>`)
}

// myTeam: state.season's own me.team, from /api/season - never the
// artifact's own g.mine flag, which prod showed marking the wrong game.
export function renderDigest(state, myTeam) {
  const head = jobHead(state)
  if (state.invalid) return section('sec-digest', head + invalidBody(state.text))
  if (!state.found) return section('sec-digest', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  const games = d.games.map(g => `<div class="sl-game${myTeam && (g.home === myTeam || g.away === myTeam) ? ' sl-game--mine' : ''}"><span class="sl-t">${esc(g.home)} vs ${esc(g.away)}</span><span class="sl-s num">${num(g.home_pts, 1)} – ${num(g.away_pts, 1)}</span>${g.top_scorers ? `<span class="sl-keep">${esc(g.top_scorers)}</span>` : ''}</div>`).join('')
  const summary = d.summary ? `<div class="sl-sec__body">${clampBlock(mdText(d.summary))}</div>` : ''
  return section('sec-digest', head + summary + `<div class="sl-games">${games}</div>
    <div class="sl-sec__body"><p class="sl-src">${mdText(d.source_note || '')}</p></div>`)
}

export function renderTrends(state) {
  const head = jobHead(state)
  if (state.invalid) return section('sec-trends', head + invalidBody(state.text))
  if (!state.found) return section('sec-trends', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  const items = d.items.map(i => {
    const text = i.player ? `<b>${esc(i.player.name)}</b> ${mdText(i.text)}` : (isSafeHref(i.link) ? `${mdText(i.text)} <a href="${esc(i.link)}" target="_blank" rel="noopener noreferrer">source</a>` : mdText(i.text))
    const who = i.player ? `<span class="sl-p">${avatarHTML(i.player, true)}<span>${clampBlock(text)}</span></span>` : clampBlock(text)
    return `<li><time>${esc(i.time)}</time><div>${who}<span class="qk-chip src">${esc(i.source)}</span></div></li>`
  }).join('')
  return section('sec-trends', head + `<div class="sl-sec__body"><ul class="sl-timeline">${items}</ul>
    <p class="sl-src">${mdText(d.source_note || '')}</p></div>`)
}

// knowMarker: knowable is optional (undefined shows nothing); a knowable
// miss is a mistake, luck is variance, not a bad call.
function knowMarker(knowable) {
  if (knowable == null) return ''
  return knowable ? '<span class="qk-badge qk-badge--err">knowable</span>' : '<span class="qk-badge">luck</span>'
}

// whyCell renders a miss/waiver_miss's optional why behind a clamped
// <details>, same pattern as a lineup row's why.
function whyCell(item) {
  return `${knowMarker(item.knowable)}${item.why ? clampBlock(mdText(item.why)) : ''}`
}

export function renderRetro(state) {
  const head = jobHead(state)
  if (state.invalid) return section('sec-retro', head + invalidBody(state.text))
  if (!state.found) return section('sec-retro', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const r = state.data
  const misses = (r.misses || []).map(m => `<tr><td>${esc(m.slot)}</td><td>${esc(m.started_name)}</td><td class="num">${num(m.started_pts, 2)}</td><td>${esc(m.better_name)}</td><td class="num">${num(m.better_pts, 2)}</td><td class="num">+${num(m.swing, 2)}</td><td>${whyCell(m)}</td></tr>`).join('')
  const waiverMisses = (r.waiver_misses || []).map(w => `<tr><td>${esc(w.name)} <small>${esc(w.pos || '')}</small></td><td class="num">${num(w.pts, 2)}</td><td>${esc(w.over_name)}</td><td class="num">${num(w.over_pts, 2)}</td><td>${whyCell(w)}</td></tr>`).join('')
  const lessons = (r.lessons || []).length ? `<div class="sl-sec__body"><p class="sl-sub">Lessons for next week</p><ul class="sl-notes">${r.lessons.map(l => `<li>${clampBlock(mdText(l))}</li>`).join('')}</ul></div>` : ''
  const swing = r.left >= Math.abs((r.opp ?? 0) - r.started) && !r.won
  return section('sec-retro', head + `<div class="sl-sec__body">
    <div class="sl-stats"><div class="sl-stat"><span>Started</span><b>${num(r.started, 2)}</b><small>${r.won ? 'won' : 'lost'}${r.opp != null ? ' to ' + num(r.opp, 2) : ''}</small></div><div class="sl-stat"><span>Best possible</span><b>${num(r.best, 2)}</b><small>from the players you owned</small></div><div class="sl-stat"><span>Left on the bench</span><b>${num(r.left, 1)}</b><small>${swing ? 'more than the margin: a winnable week' : 'less than the margin'}</small></div></div>
    ${r.summary ? clampBlock(mdText(r.summary)) : ''}
    ${misses ? `<div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>Slot</th><th>Started</th><th class="num">Pts</th><th>Better option</th><th class="num">Pts</th><th class="num">Swing</th><th>Why</th></tr></thead><tbody>${misses}</tbody></table></div>` : ''}</div>
    ${waiverMisses ? `<div class="sl-sec__body"><p class="sl-sub">Free agents who outscored your starters</p><div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>Player</th><th class="num">Pts</th><th>Instead of</th><th class="num">Pts</th><th>Why</th></tr></thead><tbody>${waiverMisses}</tbody></table></div></div>` : ''}
    ${lessons}`)
}

export function renderDraftBoard(state) {
  const head = jobHead(state)
  if (state.invalid) return section('sec-draft', head + invalidBody(state.text))
  if (!state.found) return section('sec-draft', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  if (d.report_card) return renderDraftReportCard(state)
  const slots = Object.keys(d.slots).map(Number).sort((a, b) => a - b)
  const byRound = {}
  d.picks.forEach(p => { (byRound[p.round] = byRound[p.round] || {})[p.slot] = p })
  const cell = p => {
    if (!p) return '<td></td>'
    const adp = hasADP(p.adp) ? p.adp : null
    const delta = adp ? Math.round(p.pick_no - adp) : null
    const cls = delta == null ? '' : delta >= 8 ? 'fell' : delta <= -8 ? 'reach' : ''
    return `<td class="${p.mine ? 'me' : ''}"><span class="pos ${esc(p.player.pos || '')}">${esc(p.player.pos || '')}</span>${esc(p.player.name)}<span class="adp ${cls}">${adp ? (delta > 0 ? '+' : '') + delta + ' vs ADP ' + num(adp, 0) : ''}</span></td>`
  }
  const rows = Object.keys(byRound).sort((a, b) => a - b).map(r => `<tr><td class="num">${esc(r)}</td>${slots.map(s => cell(byRound[r][s])).join('')}</tr>`).join('')
  const mine = d.picks.filter(p => p.mine)
  const fell = mine.filter(p => hasADP(p.adp) && p.pick_no - p.adp >= 8).length
  const reach = mine.filter(p => hasADP(p.adp) && p.pick_no - p.adp <= -8).length
  return section('sec-draft', head + `<div class="sl-sec__body">
    <p class="sl-summary">Your column is highlighted. Green: the player fell 8 or more picks past ADP; red: a reach of 8 or more. ${fell} value picks, ${reach} reaches.</p>
    <div class="qk-table-wrap"><table class="qk-table sl-board"><thead><tr><th>Rd</th>${slots.map(s => `<th class="${s === d.my_slot ? 'me' : ''}">${s}. ${esc(d.slots[s])}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div>
    <p class="sl-src">ADP: Sleeper PPR ADP at draft time; players Sleeper had no ADP for show none. ${esc(d.type || '')} · ${int(d.rounds)} rounds.</p></div>`)
}

export function renderDraftReportCard(state) {
  const head = jobHead(state)
  if (state.invalid) return section('sec-draft', head + invalidBody(state.text))
  if (!state.found) return section('sec-draft', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  const rows = (d.report_card || []).map(c => `<tr><td class="num">${int(c.round)}</td><td class="num">${int(c.pick)}</td><td><span class="sl-p">${avatarHTML(c.player, true)}<span>${esc(c.player.name)} <span class="sl-keep">${esc(c.player.pos || '')}</span></span></span></td><td class="num">${esc(c.drafted_as || '')}</td><td class="num">${esc(c.finished || '–')}</td><td class="num">${num(c.pts, 1)}</td><td><span class="qk-badge ${c.verdict === 'steal' ? 'qk-badge--ok' : c.verdict === 'reach' ? 'qk-badge--err' : ''}">${esc(c.verdict)}</span></td></tr>`).join('')
  const reaches = (d.report_card || []).filter(c => c.verdict === 'reach').length
  const steals = (d.report_card || []).filter(c => c.verdict === 'steal').length
  const allocText = d.pos_alloc ? Object.entries(d.pos_alloc).filter(([, n]) => n).map(([p, n]) => `${n} ${p}`).join(', ') : ''
  return section('sec-draft', head + `<div class="sl-sec__body">
    <p class="sl-summary">${steals} steals, ${reaches} reaches.${allocText ? ` Positional mix: ${esc(allocText)}.` : ''}</p>
    <div class="qk-table-wrap"><table class="qk-table"><thead><tr><th class="num">Rd</th><th class="num">Pick</th><th>Player</th><th class="num">Drafted as</th><th class="num">Finished</th><th class="num">Season pts</th><th>Verdict</th></tr></thead><tbody>${rows}</tbody></table></div>
    <p class="sl-src">Positional finish from Sleeper season stats (PPR). Reach = finished 8+ spots below where the position was taken; steal = 4+ above.</p></div>`)
}

export function renderDraftSide(state) {
  const head = jobHead(state)
  if (state.invalid) return section('draft-side', head + invalidBody(state.text))
  if (!state.found) return section('draft-side', head + emptyBody(state.job, state.title, state.what, state.running, state.runnable))
  const d = state.data
  if (!d.clock && !d.plan) return ''
  let html = ''
  if (d.clock) html = section('draft-clock', `<div class="sl-sec__head"><h2>On the clock</h2><div class="sl-sec__meta"><span>${esc(state.agent || '')}</span></div></div><div class="sl-sec__body">
    <div class="sl-clock"><b>Pick ${int(d.clock.pick_no)} (${esc(d.clock.pick_label || '')}) · ${int(d.clock.seconds_left)}s left</b>${mdText(d.clock.note)}</div>
    <p class="sl-src">Live drafts poll the picks feed every few seconds while you are within three picks of the clock.</p></div>`)
  if (d.plan) html += section('draft-plan', `<div class="sl-sec__head"><h2>Draft plan</h2><div class="sl-sec__meta"><span>${esc(state.agent || '')} · pre-draft</span></div></div><div class="sl-sec__body"><div class="sl-tiers">${d.plan.map(p => `<div><b>${esc(p.title)}</b><span>${mdText(p.note)}</span></div>`).join('')}</div></div>`)
  return html
}

export function renderReview(state) {
  const head = jobHead(state)
  if (state.invalid) return section('sec-review', head + invalidBody(state.text))
  if (!state.found) return section('sec-review', head + `<div class="sl-sec__body sl-empty"><span>Written after week 17. Until then, the week cards and hindsight carry the season.</span></div>`)
  const x = state.data
  const weeks = x.weeks || []
  const max = Math.max(1, ...weeks.map(w => w.best))
  const bars = weeks.map(w => `<div class="${w.won ? '' : 'loss'}" data-stop="${int(w.week)}" title="Week ${int(w.week)}: started ${num(w.started, 1)}, best ${num(w.best, 1)}${w.opp != null ? ', opponent ' + num(w.opp, 1) : ''}" style="height:${100 * w.best / max}%"><i style="height:${100 * w.started / w.best}%"></i></div>`).join('')
  const worst = weeks.length ? weeks.slice().sort((a, b) => (b.left ?? 0) - (a.left ?? 0))[0] : null
  return section('sec-review', head + `<div class="sl-sec__body">
    <div class="sl-stats"><div class="sl-stat"><span>Record</span><b>${int(x.wins)}-${int(x.losses)}</b><small>from draft slot ${int(x.draft_slot)}</small></div><div class="sl-stat"><span>Points for</span><b>#${int(x.pf_rank)}</b><small>${num(x.pf, 0)} · rank</small></div><div class="sl-stat"><span>Points against</span><b>#${int(x.pa_rank)}</b><small>strength of schedule</small></div><div class="sl-stat"><span>Lineup efficiency</span><b>${num(x.eff, 1)}%</b><small>${num(x.left_total, 0)} pts left on the bench</small></div><div class="sl-stat"><span>Close losses</span><b>${int(x.close_losses)}</b><small>by fewer than 10</small></div><div class="sl-stat"><span>Moves</span><b>${int(x.moves)}</b><small>waivers and trades</small></div></div>
    <h3 style="font-size:.8125rem;margin:0 0 .25rem">Weekly: started (filled) vs best possible (bar); red is a loss.</h3>
    <div class="sl-bars">${bars}</div><div class="sl-bars-x">${weeks.map(w => `<span>${int(w.week)}</span>`).join('')}</div>
    ${worst ? `<p class="sl-src">Worst week: ${int(worst.week)}, ${num(worst.left, 1)} points left on the bench${worst.won ? '' : ' in a loss'}. Champion: ${esc(x.champion || '–')}.</p>` : ''}</div>`)
}

export function renderSeasonNotes(state) {
  const head = jobHead(state)
  if (state.invalid) return section('side-notes', head + invalidBody(state.text))
  if (!state.found) {
    const body = state.running
      ? `<div class="sl-sec__body sl-empty"><span>Working on the season notes. The list fills in when the run finishes.</span></div>`
      : `<div class="sl-sec__body sl-empty"><span>No season notes yet.</span></div>`
    return section('side-notes', head + body)
  }
  const d = state.data
  return section('side-notes', head + `<div class="sl-sec__body"><ul class="sl-notes">${(d.notes || []).map(n => `<li>${clampBlock(mdText(n))}</li>`).join('')}</ul></div>`)
}

export function renderSeasonAtGlance(x) {
  if (!x) return ''
  return section('side-season', `<div class="sl-sec__head"><h2>${esc(x.season)} at a glance</h2></div><div class="sl-sec__body"><dl class="sl-kv"><dt>Record</dt><dd>${int(x.wins)}-${int(x.losses)}</dd><dt>Points for</dt><dd>${num(x.pf, 0)} · #${int(x.pf_rank)}</dd><dt>Points against</dt><dd>${num(x.pa, 0)} · #${int(x.pa_rank)}</dd><dt>Lineup eff.</dt><dd>${num(x.eff, 1)}%</dd><dt>Champion</dt><dd>${esc(x.champion || '–')}</dd></dl></div>`)
}

export function renderCrossSeason(x) {
  const items = x?.cross_season_summary
  if (!items?.length) return ''
  return section('side-cross', `<div class="sl-sec__head"><h2>Recurring patterns</h2></div><div class="sl-sec__body"><ul class="sl-mistakes">${items.map(i => `<li><b>${esc(i.n)}</b>${clampBlock(`${mdText(i.text)}${i.detail ? '<br><small>' + mdText(i.detail) + '</small>' : ''}`)}</li>`).join('')}</ul></div>`)
}

export function renderStandingsSide(season) {
  const s = season.standings || []
  const playoffLine = int(season.playoff_line) || 6
  // The dashed marker sits on the row AFTER the last playoff team (border-top
  // on that row draws it below team #playoffLine, not above it).
  return section('side-standings', `<div class="sl-sec__head"><h2>Standings</h2><span class="sl-sec__meta">${playoffLine} make the playoffs</span></div><div class="sl-sec__body"><ol class="sl-rows">${s.map((t, i) => `<li class="${i === playoffLine ? 'line' : ''}"><span class="rank">${i + 1}</span><span class="main"><b>${esc(t.team)}${t.mine ? ' <span class="qk-badge">you</span>' : ''}</b><span>${esc(t.owner)}</span></span><span class="n"><b>${record(t)}</b>${num(t.pf, 2)}</span></li>`).join('')}</ol></div>`)
}

export function renderMovesSide(season) {
  const m = season.recent_moves || []
  return section('side-moves', `<div class="sl-sec__head"><h2>Recent moves</h2></div><div class="sl-sec__body"><ul class="sl-rows">${m.map(t => `<li class="two"><span class="main"><b>${esc(t.label)}</b><span>${esc((t.by || []).join(' and '))}</span></span><span class="n">wk ${int(t.week)}</span></li>`).join('')}</ul></div>`)
}

export function emptyState(job, title, agent, what, runnable = true) {
  return section(`sec-${job}`, jobHead({ title, agent, status: 'not_run' }) + emptyBody(job, title, what, false, runnable))
}

// artifactCounts: {stop: number of artifacts found}, for the rail's dots.
export function renderTimeline(currentStop, weekNow, isCurrentSeason, artifactCounts = {}) {
  const stops = ['draft', ...Array.from({ length: 17 }, (_, i) => String(i + 1)), 'review']
  const buttons = stops.map(s => {
    const isNow = isCurrentSeason && s === String(weekNow)
    const label = s === 'draft' ? 'Draft' : s === 'review' ? 'Review' : s
    const small = s === 'draft' || s === 'review' ? '' : isNow ? 'now' : Number(s) >= 15 ? 'playoff' : ''
    const dots = Math.min(artifactCounts[s] || 0, 5)
    return `<button class="sl-stop ${isNow ? 'now' : ''} ${s === 'draft' || s === 'review' ? 'wide' : ''}" aria-pressed="${s === currentStop}" data-stop="${esc(s)}"><small>${small}</small>${label}<span class="dots">${'<i></i>'.repeat(dots)}</span></button>`
  }).join('')
  return `<nav class="sl-rail" aria-label="Season timeline">${buttons}</nav>`
}

export function renderYears(seasons, currentSeason) {
  return `<nav class="sl-years" aria-label="Seasons">${seasons.map(s => `<button class="sl-year" data-season="${esc(s.season)}" aria-pressed="${s.season === currentSeason}">${esc(s.season)}<small>${int(s.wins)}-${int(s.losses)}</small></button>`).join('')}</nav>`
}

// items: [{job, label, agent, running, disabled}]; the kebab menu's list body, split out so it can be storied.
export function renderMenuList(heading, items) {
  const rows = items.map(i => `<li><button data-run="${esc(i.job)}" ${i.disabled || i.running ? 'disabled' : ''}${i.title ? ` title="${esc(i.title)}"` : ''}>${esc(i.label)}<span>${esc(i.agent || '')}</span></button></li>`).join('')
  return `<li class="hd">${esc(heading)}</li>${rows}`
}

const KEBAB_SVG = '<svg width="20" height="20" viewBox="0 -960 960 960" fill="currentColor" aria-hidden="true"><path d="M480-160q-33 0-56.5-23.5T400-240q0-33 23.5-56.5T480-320q33 0 56.5 23.5T560-240q0 33-23.5 56.5T480-160Zm0-240q-33 0-56.5-23.5T400-480q0-33 23.5-56.5T480-560q33 0 56.5 23.5T560-480q0 33-23.5 56.5T480-400Zm0-240q-33 0-56.5-23.5T400-720q0-33 23.5-56.5T480-800q33 0 56.5 23.5T560-720q0 33-23.5 56.5T480-640Z"/></svg>'

// The full kebab menu, open, for stories - the served page's own
// index.html markup drives the real <details> (see main.js's renderMenu).
export function kebabMenuHTML(heading, items) {
  return `<details class="sl-menu" open><summary aria-label="Run a job" title="Run a job">${KEBAB_SVG}</summary><ul class="sl-menu__list">${renderMenuList(heading, items)}</ul></details>`
}
