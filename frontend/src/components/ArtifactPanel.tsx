import { createElement, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeRaw from 'rehype-raw'
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize'
import rehypeHighlightSubset from '../lib/rehypeHighlightSubset'
import 'highlight.js/styles/github-dark.css'
import type { Element } from 'hast'
import { api, artifactUrl } from '../api'
import type { ArtifactSummary, ArtifactRevisionInfo } from '../api'
import { CopyButton } from './CopyButton'
import { CopyablePre } from './CopyablePre'
import { escapeUnmatchedBackticks } from '../lib/backticks'
import { useChatStore } from '../state/ChatStoreProvider'
import { Icon } from './Icon'

// JudgeRoundContent is the JSON body of a `judge_round` artifact (design V4
// §4.3) - the only place a note's line anchor lives. Fetched and parsed
// client-side; not part of the generated schema (it's an artifact body, not a REST response shape).
interface JudgeNoteRef {
  artifact_id: string
  revision: number
  line_hint?: number
  snippet?: string
}
interface JudgeNote {
  ref: JudgeNoteRef
  text: string
  criterion?: string
}
interface JudgeRoundContent {
  turn?: string
  round?: number
  passed?: boolean
  score?: number
  // The artifact revisions this round judged (server shape: internal/vetting/reviewrecord.go's
  // ScoredRef, json "scored"). A round can score several artifacts; the
  // timeline uses the ref that points at the node's primary output.
  scored?: { artifact_id: string; revision: number }[]
  notes?: JudgeNote[]
  // criteria/evidence: only JudgeRoundView reads these; the timeline chip
  // and note-anchoring above use round/passed/score/notes only.
  criteria?: { name?: string; score?: number; feedback?: string }[]
  evidence?: { probes?: { name?: string; result?: string }[] }
}

// The other three typed-view shapes - only their own view component reads these.
export interface CodeReviewBody {
  verdict?: string
  takeaway?: string
  verified?: string[]
  notes?: string[]
  finding_ids?: string[]
  dismissed?: string[]
}
export interface FindingBody {
  path?: string
  line_hint?: number
  snippet?: string
  title?: string
  rationale?: string
  severity?: string
  state?: string
}
export interface PlanBody {
  plan_id?: string
  assignments?: { node_id?: string; task?: string }[]
  setup?: { repo?: string; base_ref?: string; work_branch?: string }
  delivery?: { kind?: string }
  status?: string
}

interface AnchorResult {
  byLine: Map<number, JudgeNote[]>
  unanchored: JudgeNote[]
}

// Locates each note in the shown revision: an exact substring match on its
// quoted snippet wins; line_hint is the fallback (a line shift, a stale
// snippet after an edit); neither match is unanchored (design V4 §9).
export function anchorNotes(lines: string[], notes: JudgeNote[]): AnchorResult {
  const byLine = new Map<number, JudgeNote[]>()
  const unanchored: JudgeNote[] = []
  for (const note of notes) {
    let idx = -1
    if (note.ref.snippet) {
      // A multi-line quoted snippet can never satisfy includes() against a
      // single line - anchor on just its first line instead (same
      // first-occurrence risk profile a single-line match already has).
      const firstLine = note.ref.snippet.split('\n')[0]
      idx = lines.findIndex(l => l.includes(firstLine))
    }
    if (idx === -1 && note.ref.line_hint != null) {
      const hinted = note.ref.line_hint - 1 // line_hint is 1-based
      if (hinted >= 0 && hinted < lines.length) idx = hinted
    }
    if (idx === -1) {
      unanchored.push(note)
      continue
    }
    const existing = byLine.get(idx) ?? []
    existing.push(note)
    byLine.set(idx, existing)
  }
  return { byLine, unanchored }
}

// Run bookkeeping, never a node's own deliverable: dag_node is rewritten by
// the system at node start/end, dag_plan belongs to the orchestrator, and a
// dispatch-authored bytes:* blob is the run's own input staging, not anything this node produced.
export function isBookkeeping(a: { kind?: string; name: string }): boolean {
  return a.kind === 'dag_node' || a.kind === 'dag_plan' || a.name.startsWith('bytes:')
}

// kindRank tiers a candidate: review, then the node's declared kind, then a
// blob deliverable, then a finding; anything else is a last resort.
function kindRank(a: ArtifactSummary, nodeArtifactKind?: string): number {
  if (a.kind === 'code_review') return 0
  if (nodeArtifactKind && a.kind === nodeArtifactKind) return 1
  if (a.class !== 'structured') return 2
  if (a.kind === 'finding') return 3
  return 4
}

// The node's RESULT (#1178 - there is no picker): computed via kindRank,
// with revision/saved_at/name breaking a tie within one tier.
export function selectPrimaryOutput(artifacts: ArtifactSummary[], nodeArtifactKind?: string, focusArtifactId?: string): ArtifactSummary | null {
  const candidates = artifacts.filter(a => a.kind !== 'judge_round' && !isBookkeeping(a))
  if (candidates.length === 0) return null
  // The focus hint wins outright when it names one of THIS node's artifacts -
  // it's "show what was tapped," a stronger signal than kind rank or the
  // newest-wins default (#1250) - a secondary tap inside the panel reuses this same mechanism.
  const focused = focusArtifactId ? candidates.find(a => a.name === focusArtifactId) : undefined
  if (focused) return focused
  return candidates.reduce((best, a) => (compareOutput(a, best, nodeArtifactKind) > 0 ? a : best))
}

function compareOutput(a: ArtifactSummary, b: ArtifactSummary, nodeArtifactKind?: string): number {
  // Positive when `a` beats `b`: lower kindRank first, then newer output
  // (latest_revision), then later-saved lineage, then the alphabetically
  // EARLIER name as the final deterministic tiebreak.
  const ar = kindRank(a, nodeArtifactKind)
  const br = kindRank(b, nodeArtifactKind)
  if (ar !== br) return br - ar
  const rr = (a.latest_revision ?? 0) - (b.latest_revision ?? 0)
  if (rr !== 0) return rr
  const as = a.lineage?.saved_at ?? ''
  const bs = b.lineage?.saved_at ?? ''
  if (as !== bs) return as > bs ? 1 : -1
  return a.name > b.name ? -1 : a.name < b.name ? 1 : 0
}

// firstLine is a blob's title fallback: its first non-blank line, heading
// markup stripped - also the empty state's "name the delivery" text when there is no artifact at all.
export function firstLine(text: string): string | null {
  const line = text.split('\n').find(l => l.trim() !== '')
  return line ? line.trim().replace(/^#+\s*/, '') : null
}

// severityLabel/findingLoc are shared between the finding title and FindingView.
function findingLoc(f: FindingBody): string | undefined {
  return f.path ? (f.line_hint != null ? `${f.path}:${f.line_hint}` : f.path) : undefined
}

// artifactTitle names an artifact for a human, never its raw id. `body` is
// the parsed JSON for a structured kind, or the raw text for a blob.
function reviewTitle(body: unknown): string {
  const r = (body ?? {}) as CodeReviewBody
  const n = r.finding_ids?.length ?? 0
  return `Review · ${r.verdict ?? '?'} · ${n} finding${n === 1 ? '' : 's'}`
}
function findingTitle(summary: ArtifactSummary, body: unknown): string {
  const f = (body ?? {}) as FindingBody
  return [f.severity, findingLoc(f)].filter(Boolean).join(' · ') || summary.name
}
function judgeRoundTitle(body: unknown): string {
  const j = (body ?? {}) as JudgeRoundContent
  const parts = [`Judge round ${j.round ?? '?'}`]
  if (j.score != null) parts.push(j.score.toFixed(2))
  parts.push(j.passed ? 'passed' : 'failed')
  return parts.join(' · ')
}

export function artifactTitle(summary: ArtifactSummary, body: unknown): string {
  switch (summary.kind) {
    case 'code_review': return reviewTitle(body)
    case 'finding': return findingTitle(summary, body)
    case 'judge_round': return judgeRoundTitle(body)
    default: return typeof body === 'string' ? (firstLine(body) ?? summary.name) : summary.name
  }
}

// The revision a judge round judged for the given artifact - from the
// round's scored list (keyed by artifact id, a round can score several) -
// falling back to the artifact's latest revision when unscored.
export function resolveScoredRevision(body: JudgeRoundContent, primaryId: string, fallback: number): number {
  return body.scored?.find(s => s.artifact_id === primaryId)?.revision ?? fallback
}

// makeActiveRoundEffect builds the "tapped chip moves the cursor" effect
// body (not the click handler): a tap can land BEFORE the revision list has
// arrived, since both fetches start together on open.
function makeActiveRoundEffect(activeRoundId: string | null, judgeBodies: Record<string, JudgeRoundContent>, primaryId: string | null, revisions: ArtifactRevisionInfo[], latestRev: number | null, revIdx: number | null, setRevIdx: (i: number) => void) {
  return () => {
    if (activeRoundId == null || primaryId == null || revisions.length === 0) return
    const b = judgeBodies[activeRoundId]
    if (!b) return
    const rev = resolveScoredRevision(b, primaryId, latestRev ?? revisions[revisions.length - 1].revision)
    const idx = revisions.findIndex(x => x.revision === rev)
    const target = idx >= 0 ? idx : revisions.length - 1
    if (target !== revIdx) setRevIdx(target)
  }
}
// (openapi.yaml's ArtifactRevisionList) into the ascending list the panel's
// numeric cursor walks through.
export function toAscending(revs: ArtifactRevisionInfo[]): ArtifactRevisionInfo[] {
  return [...revs].reverse()
}

// Mirrors the diff endpoint's allowlist (internal/server/rest/artifacts.go:
// only application/json and text/* - anything else 415s), so the toggle can
// say why it's disabled BEFORE the request instead of after the error.
function isDiffableMime(mime: string): boolean {
  return mime === 'application/json' || mime.startsWith('text/')
}

// cursorFor derives the primary's revision cursor values (cursor + previous
// info, the viewed revision, the list's latest) - a pure function of the
// list and cursor index, computed every render exactly as before.
function cursorFor(revIdx: number | null, revisions: ArtifactRevisionInfo[], primary: ArtifactSummary | null) {
  const curInfo = revIdx != null ? revisions[revIdx] ?? null : null
  const prevInfo = revIdx != null && revIdx > 0 ? revisions[revIdx - 1] ?? null : null
  const currentRev = curInfo?.revision ?? null
  const latestRev = revisions.length > 0 ? revisions[revisions.length - 1]?.revision ?? null : primary?.latest_revision ?? null
  return { curInfo, prevInfo, currentRev, latestRev }
}

// diffBlockReason is the Diff toggle's visible disabled reason - no cursor,
// no previous revision, a binary mime on either side (the endpoint 415s),
// or the server rejected the pair as too large (413).
function diffBlockReason(curInfo: ArtifactRevisionInfo | null, prevInfo: ArtifactRevisionInfo | null, diffBlocked: string | null): string | null {
  if (curInfo == null) return 'No revision to diff'
  if (prevInfo == null) return 'No previous revision'
  if (!isDiffableMime(curInfo.mime_type) || !isDiffableMime(prevInfo.mime_type)) return 'Only text and JSON revisions can be diffed'
  return diffBlocked
}

// displayState derives what the shared renderer stack needs from the
// cursor's content: the pretty-printed display text and the structured
// flag (the JSON parse itself stays memoized at the call site).
function displayState(content: string | null, primary: ArtifactSummary | null) {
  const klass = primary == null ? undefined : primary.class
  return { displayText: content != null ? prettyText(content, klass) : null, isStructured: klass === 'structured' }
}

// triggerActivation returns the Details "trigger" row's jump handler -
// only when that trigger's judge round is one of this node's (a known
// chip); undefined renders it as plain text.
function triggerActivation(curInfo: ArtifactRevisionInfo | null, judgeBodies: Record<string, JudgeRoundContent>, activateRound: (id: string) => void) {
  const triggerId = curInfo?.lineage?.trigger_annotation
  return triggerId != null && judgeBodies[triggerId] != null ? () => activateRound(triggerId) : undefined
}

interface Props {
  chatId: string
  nodeId: string
  // The node's display name - the panel's <h2>. Same agentLabel() DagNode's
  // card header shows (#1216 review): the raw prompt (nodeTask) can run to a
  // kilobyte and blew the header past the sheet height when shown here - it now lives only in the Details "Task" row.
  nodeAgent: string
  // The node's raw prompt - shown in Details, never the header (see above).
  nodeTask: string
  // The node's error text (NodeState.error). Drives the failure empty state
  // when the node has no non-judge artifact.
  nodeError?: string
  // The node's own vetted answer text - the empty state's "name the
  // delivery" fallback when the node wrote no artifact at all, e.g. an ACP implementer that delivers through git.
  nodeAnswer?: string
  // The node's declared output kind (DagNodeDef.artifact) - makes the
  // primary-output selection exact. Absent when the node declares none.
  nodeArtifactKind?: string
  // A FOCUS HINT, not a picker (#1178 stays intact): when the caller already
  // knows which artifact the viewer tapped (#1250's <artifacts> rows), this
  // makes THAT one the shown primary instead of the default. Ignored if it doesn't name an artifact on this node - the default selection still applies.
  focusArtifactId?: string
  onClose: () => void
}

// A result view, not a picker (#1178): opening shows the node's primary
// output under the node's own name, judge rounds as a chip timeline above it,
// a Revision N-of-M prev/next bar with a single diff toggle, a titled secondary list, and everything provenance-shaped (id, kind, class, lineage, timestamps, the REST link) in a collapsed "Details" disclosure at the bottom. Both native <select>s and the picker-era desktop sidebar are gone - nothing left to pick.
export function ArtifactPanel({ chatId, nodeId, nodeAgent, nodeTask, nodeError, nodeAnswer, nodeArtifactKind, focusArtifactId, onClose }: Props) {
  const [summaries, setSummaries] = useState<ArtifactSummary[]>([])
  const [error, setError] = useState<string | null>(null)
  // rawView: the monospace line-list is the FALLBACK view (also what a
  // judge-note highlight needs a stable line index for - #1114 keeps it as a
  // toggle rather than dropping it), not the default; structured kinds default to the collapsible tree, blobs to rendered markdown.
  const [rawView, setRawView] = useState(false)
  // activeRoundId is the tapped timeline chip (null = no chip active, every
  // round's matching notes anchor). Declared up here with the other state;
  // the activation effect below is what it drives.
  const [activeRoundId, setActiveRoundId] = useState<string | null>(null)
  // focusedOverride: an in-panel secondary tap opens the tapped item IN
  // PLACE, with no per-item bars of its own. Reuses selectPrimaryOutput's
  // own focus-hint mechanism (#1250), so the tapped item becomes THE view -
  // same revision bar, diff, raw, copy - instead of a second set of controls.
  const [focusedOverride, setFocusedOverride] = useState<string | null>(null)

  const load = useCallback(() => {
    // Returns the promise (not fire-and-forget) - withScrollPreserved below
    // awaits it to restore scroll only after the refetch actually lands.
    return api.listChatArtifacts(chatId).then(l => setSummaries(l.data ?? [])).catch(e => setError(String(e)))
  }, [chatId])
  useEffect(() => { void load() }, [load])

  // Membership is by the LATEST revision's lineage.node_id (ArtifactSummary
  // only carries that revision's lineage - see toArtifactSummary in
  // internal/server/rest/artifacts.go), not "any revision this node wrote": if a later node revises an artifact (e.g. a judge writes revision 2 of a worker's `finding`) it moves to the reviser's panel and disappears from the original author's - a real gap against a "everything this node wrote is an output" reading of design V4, open as a question on #1094's review pending a spec answer. Fixing it needs per-revision lineage (GET .../revisions) up front for every artifact in the chat, which doesn't scale to "one click opens a panel" - documented here rather than silently wrong.
  // isBookkeeping excludes dag_node/dag_plan/bytes:* entirely - they belong to the run, never to this node's own panel.
  const nodeArtifacts = useMemo(
    () => summaries.filter(s => s.lineage?.node_id === nodeId && !isBookkeeping(s)),
    [summaries, nodeId],
  )

  // The panel's one and only shown artifact: computed, not chosen. Judge
  // rounds are not candidates - they are the timeline above the output, not
  // pickable content. focusedOverride (an in-panel secondary tap) wins over
  // the caller's own focusArtifactId, same as a fresh tap always beats a stale hint.
  const primary = useMemo(
    () => selectPrimaryOutput(nodeArtifacts, nodeArtifactKind, focusedOverride ?? focusArtifactId),
    [nodeArtifacts, nodeArtifactKind, focusedOverride, focusArtifactId],
  )
  const primaryId = primary?.name ?? null

  // A different primary (list reloaded and the computed choice changed)
  // resets the whole view to the latest, un-highlighted state - the
  // picker-era "select artifact" reset, kept because a refresh can move it.
  const lastPrimaryId = useRef<string | null>(null)
  useEffect(() => {
    if (lastPrimaryId.current === primaryId) return
    lastPrimaryId.current = primaryId
    setRevisions([])
    setRevIdx(null)
    setContent(null)
    setDiffText(null)
    setDiffOn(false)
    setDiffBlocked(null)
    setDiffFailed(false)
    setActiveRoundId(null)
  }, [primaryId])

  // Judge rounds: one body fetch per round (latest content of each
  // judge_round artifact), kept in a map the timeline chips and the
  // activation effect below read from.
  const judgeIds = useMemo(
    () => nodeArtifacts.filter(a => a.kind === 'judge_round').map(a => a.name),
    [nodeArtifacts],
  )
  const [judgeBodies, setJudgeBodies] = useState<Record<string, JudgeRoundContent>>({})
  const judgeBodiesToken = useRef(0)
  const loadJudgeBodies = useCallback(() => {
    if (judgeIds.length === 0) { setJudgeBodies({}); return }
    const token = ++judgeBodiesToken.current
    return Promise.all(
      judgeIds.map(id =>
        api.getArtifactText(chatId, id)
          .then(t => JSON.parse(t) as JudgeRoundContent)
          .catch(() => null),
      ),
    ).then(bodies => {
      if (token !== judgeBodiesToken.current) return
      const map: Record<string, JudgeRoundContent> = {}
      judgeIds.forEach((id, i) => { const b = bodies[i]; if (b) map[id] = b })
      setJudgeBodies(map)
    })
  }, [chatId, judgeIds])
  useEffect(() => { void loadJudgeBodies() }, [loadJudgeBodies])

  // The timeline: one chip per parsed judge body, round order.
  const chips = useMemo(() => {
    const entries = Object.entries(judgeBodies).map(([id, b]) => ({ id, b }))
    entries.sort((x, y) => (x.b.round ?? 0) - (y.b.round ?? 0))
    return entries
  }, [judgeBodies])

  // Secondary artifacts: everything but the primary and the judge rounds
  // (the timeline above handles those). A plain list, opened in place - no per-item groups/bars.
  const secondaryItems = useMemo(
    () => nodeArtifacts.filter(a => a.kind !== 'judge_round' && a.name !== primaryId),
    [nodeArtifacts, primaryId],
  )
  // Secondary bodies: every secondary artifact's OWN latest_revision body
  // (explicit, not an implicit "no revision" fetch - matches exactly what
  // the summary already reports as latest), parsed per its own class. Titles
  // the secondary list (artifactTitle needs the body, not just the kind) and
  // resolves a review's finding_ids inline - both need every candidate's content up front, not lazily per tap.
  const [secondaryBodies, setSecondaryBodies] = useState<Record<string, unknown>>({})
  const secondaryBodiesToken = useRef(0)
  const loadSecondaryBodies = useCallback(() => {
    if (secondaryItems.length === 0) { setSecondaryBodies({}); return }
    const token = ++secondaryBodiesToken.current
    return Promise.all(
      secondaryItems.map(a =>
        api.getArtifactText(chatId, a.name, a.latest_revision)
          .then(t => tryParseJSON(t) ?? t)
          .catch(() => undefined),
      ),
    ).then(bodies => {
      if (token !== secondaryBodiesToken.current) return
      const map: Record<string, unknown> = {}
      secondaryItems.forEach((a, i) => { if (bodies[i] !== undefined) map[a.name] = bodies[i] })
      setSecondaryBodies(map)
    })
  }, [chatId, secondaryItems])
  useEffect(() => { void loadSecondaryBodies() }, [loadSecondaryBodies])

  // Revisions of the primary, ascending (the endpoint returns
  // newest-first) - the cursor indexes this list.
  const [revisions, setRevisions] = useState<ArtifactRevisionInfo[]>([])
  const [revIdx, setRevIdx] = useState<number | null>(null)
  const { curInfo, prevInfo, currentRev, latestRev } = cursorFor(revIdx, revisions, primary)

  const revisionsToken = useRef(0)
  // Read via a ref (not a useCallback dep) so a cursor MOVE never
  // re-fetches the list: refresh() calls this directly and wants the
  // cursor as it is NOW, while the effect only re-fires on a new primary.
  const currentRevRef = useRef(currentRev)
  currentRevRef.current = currentRev
  const loadRevisions = useCallback((opts?: { toLatest?: boolean }) => {
    if (!primaryId) { setRevisions([]); setRevIdx(null); return }
    const token = ++revisionsToken.current
    return api.listArtifactRevisions(chatId, primaryId)
      .then(r => {
        if (token !== revisionsToken.current) return
        setError(null)
        const asc = toAscending(r.data ?? [])
        setRevisions(asc)
        // Keep the currently viewed revision across a refresh if it still
        // exists; a fresh primary, a vanished revision, or an explicit
        // toLatest (live-follow) lands on latest. toLatest is passed explicitly rather than via currentRevRef - a ref the render body also writes can be clobbered by a re-render that lands between the intent being set and this read.
        const want = opts?.toLatest ? null : currentRevRef.current
        const wantIdx = want != null ? asc.findIndex(x => x.revision === want) : -1
        setRevIdx(wantIdx >= 0 ? wantIdx : asc.length - 1)
      })
      .catch(e => { if (token === revisionsToken.current) setError(String(e)) })
  }, [chatId, primaryId])
  useEffect(() => { void loadRevisions() }, [loadRevisions])

  // The tapped chip's judged revision, as an effect (not the click handler):
  // a tap can land BEFORE the revision list has arrived (both fetches start
  // together on open). move() clears activeRoundId before the cursor changes, so this never fights a manual prev/next.
  const activeBody = activeRoundId != null ? judgeBodies[activeRoundId] : null
  useEffect(makeActiveRoundEffect(activeRoundId, judgeBodies, primaryId, revisions, latestRev, revIdx, setRevIdx), [activeRoundId, judgeBodies, revisions])

  // Content of the cursor's revision. A chip tap that targets an as-yet
  // unloaded revision reaches this same path: revIdx moves, this fires the
  // getArtifactText fetch; the token ref below keeps a slow prior response from clobbering it (stale-response guard the picker era built, kept - artifact switches are gone, refreshes remain).
  const [content, setContent] = useState<string | null>(null)
  const [activeNote, setActiveNote] = useState<JudgeNote | null>(null)
  const contentToken = useRef(0)
  const loadContent = useCallback(() => {
    setActiveNote(null)
    if (!primaryId || currentRev == null) { setContent(null); return }
    const token = ++contentToken.current
    return api.getArtifactText(chatId, primaryId, currentRev)
      .then(text => { if (token !== contentToken.current) return; setError(null); setContent(text) })
      .catch(e => { if (token === contentToken.current) setError(String(e)) })
  }, [chatId, primaryId, currentRev])
  useEffect(() => { void loadContent() }, [loadContent])

  // Diff, always against the PREVIOUS revision (no "against" picker): one
  // toggle. Disabled with a visible reason when there is no previous
  // revision, either side is a binary mime (endpoint 415 - checked here against the revision list's own mime_type, mirroring the server's allowlist), or the server has rejected this pair as too large (413 over the 256KB bound).
  const [diffOn, setDiffOn] = useState(false)
  const [diffBlocked, setDiffBlocked] = useState<string | null>(null)
  const [diffFailed, setDiffFailed] = useState(false)
  const [diffText, setDiffText] = useState<string | null>(null)
  const diffDisabledReason = diffBlockReason(curInfo, prevInfo, diffBlocked)
  const diffActive = diffOn && diffDisabledReason == null

  const diffToken = useRef(0)
  const loadDiff = useCallback(() => {
    if (!diffActive) {
      setDiffText(null)
      // Clear a failed-diff error only when a DIFF fetch actually set it:
      // a content-fetch failure shares the same banner and must survive a
      // diff toggle.
      if (diffFailed) { setError(null); setDiffFailed(false) }
      return
    }
    const token = ++diffToken.current
    return api.diffArtifactRevisions(chatId, primaryId!, prevInfo!.revision, curInfo!.revision)
      .then(text => { if (token !== diffToken.current) return; setError(null); setDiffText(text) })
      .catch((e: unknown) => {
        if (token !== diffToken.current) return
        setDiffText(null)
        setDiffOn(false)
        const status = (e as { status?: number })?.status
        if (status === 413) {
          // The server's 413 message embeds this artifact's id - show the
          // panel's own reason instead, since the id may only appear in
          // Details.
          setDiffBlocked('Too large to diff (256 KB limit)')
        } else if (status === 415) {
          setDiffBlocked('Only text and JSON revisions can be diffed')
        } else {
          setError(String(e))
          setDiffFailed(true)
        }
      })
  }, [diffActive, chatId, primaryId, prevInfo, curInfo, diffFailed])
  useEffect(() => { void loadDiff() }, [loadDiff])

  // Judge notes for what's on screen: a tapped chip contributes its OWN
  // round's notes for the primary at the revision that round judged; with no
  // chip active, every round's matching notes anchor (#1139, kept). The anchoring machinery - anchorNotes -> notesInRange -> the renderers - is untouched.
  const notes = useMemo(() => {
    if (!primary || currentRev == null) return []
    const roundBodies = activeBody ? [activeBody] : Object.values(judgeBodies)
    return roundBodies
      .flatMap(b => b.notes ?? [])
      .filter(n => n.ref.artifact_id === primary.name && n.ref.revision === currentRev)
  }, [primary, currentRev, activeBody, judgeBodies])

  // refresh re-runs every fetch the panel currently has live: the artifact
  // list, the judge bodies, and - when there is a primary - its
  // revisions/content/diff.
  const refresh = useCallback(() => {
    void load()
    void loadJudgeBodies()
    void loadSecondaryBodies()
    void loadRevisions()
    void loadContent()
    void loadDiff()
  }, [load, loadJudgeBodies, loadSecondaryBodies, loadRevisions, loadContent, loadDiff])

  // Live SSE follow (#1114): chatStore.subscribe already fans out to any
  // listener while mounted (same seam DagNode/NodePopup use) - no new pub/sub.
  // Refs, not deps: the listener is registered once per chatId and must read state as it is at event time, not subscribe time.
  const store = useChatStore()
  const primaryIdRef = useRef(primaryId)
  primaryIdRef.current = primaryId
  const nodeArtifactNamesRef = useRef<Set<string>>(new Set())
  nodeArtifactNamesRef.current = useMemo(() => new Set(nodeArtifacts.map(a => a.name)), [nodeArtifacts])
  // atLatestRef: true when the cursor is on the newest revision, so a fresh
  // one should pull the view forward; false (pinned to an older revision by
  // the user) means the refetch must leave the cursor where it is.
  const atLatestRef = useRef(true)
  useEffect(() => {
    atLatestRef.current = revIdx == null || revisions.length === 0 || revIdx === revisions.length - 1
  }, [revIdx, revisions])
  const seenSeqRef = useRef(0)
  const scrollRef = useRef<HTMLDivElement>(null)
  const withScrollPreserved = useCallback((run: () => void | Promise<unknown>) => {
    const el = scrollRef.current
    const top = el?.scrollTop
    void Promise.resolve(run()).then(() => {
      requestAnimationFrame(() => {
        if (scrollRef.current && top != null) scrollRef.current.scrollTop = top
      })
    })
  }, [])
  useEffect(() => {
    return store.subscribe(chatId, () => {
      const ev = store.get(chatId).artifactEvents
      if (!ev || ev.seq === seenSeqRef.current) return
      seenSeqRef.current = ev.seq
      const rev = ev.revision
      if (rev && rev.nodeId === nodeId) {
        // Only the list and the primary's own revisions refetch here; a
        // secondary's title (secondaryBodies) goes stale until the next
        // manual Refresh - a deliberate scope boundary, not a bug.
        withScrollPreserved(load)
        if (rev.id === primaryIdRef.current) {
          const toLatest = atLatestRef.current
          withScrollPreserved(() => loadRevisions({ toLatest }))
        }
      }
      const jr = ev.judgeRound
      if (jr && jr.scored.some(s => s.artifactId === primaryIdRef.current || nodeArtifactNamesRef.current.has(s.artifactId))) {
        withScrollPreserved(load)
      }
    })
  }, [store, chatId, nodeId, load, loadRevisions, withScrollPreserved])

  function move(delta: 1 | -1) {
    if (revIdx == null || revisions.length === 0) return
    const next = revIdx + delta
    if (next < 0 || next >= revisions.length) return
    setRevIdx(next)
    // Notes are anchored to the round's judged revision - a manual move
    // drops them rather than highlighting lines the round never judged.
    setActiveRoundId(null)
    setDiffBlocked(null)
    const target = revisions[next]
    const before = revisions[next - 1]
    if (next === 0 || !isDiffableMime(target.mime_type) || !isDiffableMime(before.mime_type)) setDiffOn(false)
  }

  function activateRound(id: string) {
    const b = judgeBodies[id]
    if (!b) return
    // A chip is about ONE specific revision, a diff about ADJACENT ones -
    // showing both at once would diff a revision the user didn't ask about.
    setDiffOn(false)
    setDiffBlocked(null)
    setActiveRoundId(id)
    // The cursor itself is set by the activation effect above, which also
    // covers a tap that lands before the revision list has arrived.
  }

  const { displayText, isStructured } = displayState(content, primary)
  const lines = useMemo(() => (displayText != null ? displayText.split('\n') : []), [displayText])
  const { byLine, unanchored } = useMemo(() => anchorNotes(lines, notes), [lines, notes])
  const parsedJson = useMemo(() => (isStructured && content != null ? tryParseJSON(content) : undefined), [isStructured, content])

  // A review's finding_ids, resolved to their titles/bodies in the order
  // the review itself lists them. Only computed when the primary IS a review; harmless (and cheap) otherwise.
  const reviewFindings = useMemo(() => {
    if (primary?.kind !== 'code_review' || parsedJson == null) return []
    const ids = (parsedJson as CodeReviewBody).finding_ids ?? []
    return ids.map(id => ({ id, body: secondaryBodies[id] as FindingBody | undefined }))
  }, [primary?.kind, parsedJson, secondaryBodies])

  // Details: the one place a raw id may appear. onTrigger jumps to the
  // round that produced this revision - but only when that round is one of
  // this node's (a known chip); otherwise it renders as plain text.
  const onTrigger = triggerActivation(curInfo, judgeBodies, activateRound)

  // Native <dialog> + showModal(): Esc closes ('cancel' then 'close'), focus
  // is trapped in the top layer, and the browser restores focus to the opener
  // - no manual trap/restore. onClose (the native 'close' event, fired on Esc AND our own .close() calls below) is the single place that tells the parent to unmount us, so that restore always finishes before React removes the dialog.
  const dialogRef = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    dialogRef.current?.showModal()
  }, [])

  const empty = primary == null
  // Details renders its content only while OPEN: a closed native <details>
  // keeps its content in the DOM (hidden, still readable by text-scanning
  // tools), and the raw id must exist NOWHERE until the disclosure is opened (#1178).
  const [detailsOpen, setDetailsOpen] = useState(false)

  return (
    <dialog
      ref={dialogRef}
      aria-label={`Artifacts for node ${nodeId}`}
      onClose={onClose}
      onClick={e => { if (e.target === dialogRef.current) dialogRef.current?.close() }}
      // Below `medium`: the same bottom-sheet shell as Sheet/NodePopup (docked
      // to the bottom edge via mt-auto, rounded top, scrim, safe-area padding);
      // a centred card at medium+. dvh, not vh, so it clears mobile browser chrome (#1177). One component tree at every width - the only width-conditional code is this class.
      className="m-0 mt-auto w-screen max-w-[100vw] h-[90dvh] medium:m-auto medium:w-full medium:max-w-4xl medium:h-[min(32rem,85vh)] max-h-[100vh] medium:max-h-[85dvh] p-0 border-0 rounded-t-2xl medium:rounded-2xl bg-transparent backdrop:bg-black/40"
    >
      <div
        className="relative flex flex-col w-full h-full overflow-hidden rounded-t-2xl medium:rounded-2xl bg-gray-50 dark:bg-gray-900 shadow-xl pb-[calc(0.75rem+var(--composer-gap))] medium:pb-0"
      >
        {/* Header: the node's own name (never an artifact id or its raw
            prompt - #1216), refresh, close - non-scrolling; >=44px targets
            (#1135). line-clamp-2 is a hard ceiling: even a future caller
            that passes a long label can't blow the header past 2 lines. */}
        <header className="shrink-0 flex items-start gap-1 px-4 py-1.5 border-b border-gray-200 dark:border-gray-700">
          <h2 className="flex-1 min-w-0 py-2 text-sm font-semibold text-gray-800 dark:text-gray-100 break-words line-clamp-2">
            {nodeAgent}
          </h2>
          <div className="flex items-center gap-1 shrink-0 py-1.5">
            <button
              onClick={refresh}
              aria-label="Refresh artifacts"
              title="Refresh artifacts"
              className="flex h-11 w-11 items-center justify-center rounded-lg text-gray-500 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-400 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
            >
              <Icon name="refresh" className="w-4 h-4" />
            </button>
            <button
              onClick={() => dialogRef.current?.close()}
              aria-label="Close"
              className="flex h-11 w-11 items-center justify-center rounded-lg text-gray-500 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-400 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
            >
              <Icon name="close" className="w-4 h-4" />
            </button>
          </div>
        </header>

        <TimelineBlock chips={chips} hidden={empty} activeRoundId={activeRoundId} onActivate={activateRound} />

        {/* The single scrolling region: revision bar, the rendered output
            (with judge-note highlights), More, Details. */}
        <div ref={scrollRef} className="flex-1 min-h-0 overflow-y-auto overscroll-contain px-4 medium:px-5 py-3 space-y-3">
          <ErrorLine error={error} />

          {empty ? <EmptyState nodeError={nodeError} deliveryText={nodeAnswer ? firstLine(nodeAnswer) : null} /> : (
            <>
              {/* The focused artifact's title - a human name, not its
                  raw id; the raw id stays inside
                  Details. Loading (parsedJson/content still null) falls back to the artifact's own name via artifactTitle. */}
              <p className="text-xs font-medium text-gray-700 dark:text-gray-200 break-words">
                {artifactTitle(primary, isStructured ? parsedJson : content)}
              </p>

              <RevisionBar
                currentRev={currentRev}
                count={revisions.length}
                revIdx={revIdx}
                diffActive={diffActive}
                diffDisabledReason={diffDisabledReason}
                onPrev={() => move(-1)}
                onNext={() => move(1)}
                onToggleDiff={() => setDiffOn(d => !d)}
                rawView={rawView}
                onToggleRaw={() => setRawView(r => !r)}
                displayText={displayText}
              />

              <PrimaryView
                diffActive={diffActive}
                diffText={diffText}
                content={content}
                displayText={displayText}
                lines={lines}
                rawView={rawView}
                isStructured={isStructured}
                parsedJson={parsedJson}
                kind={primary?.kind}
                byLine={byLine}
                activeNote={activeNote}
                onSelectNote={setActiveNote}
                reviewFindings={reviewFindings}
              />

              <ActiveNoteCallout note={activeNote} />

              <UnanchoredNotes notes={unanchored} />

              <SecondaryList items={secondaryItems} bodies={secondaryBodies} onSelect={setFocusedOverride} />

              <DetailsSection
                curInfo={curInfo}
                open={detailsOpen}
                onOpenChange={setDetailsOpen}
                nodeTask={nodeTask}
                primary={primary}
                revisionCount={revisions.length}
                onTrigger={onTrigger}
                chatId={chatId}
              />
            </>
          )}
        </div>
      </div>
    </dialog>
  )
}

// prettyText pretty-prints a JSON structured artifact; returns the bytes
// unchanged for a blob (markdown/text) - both render as a plain line list so
// judge-note highlighting has a stable line number to anchor on.
function prettyText(raw: string, klass?: string): string {
  if (klass !== 'structured') return raw
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

// The tree view's own parse (separate from prettyText's, which already
// swallows a parse error into a raw-text fallback) - returns undefined rather
// than throwing so the tree view falls back to raw text for unexpectedly invalid JSON.
function tryParseJSON(raw: string): unknown {
  try {
    return JSON.parse(raw)
  } catch {
    return undefined
  }
}

// SecondaryList: every non-primary artifact as one plain, titled row - no
// per-item revision bar, Raw toggle, or copy button. Tapping a row makes IT
// the focused/primary view (the shared RevisionBar, Raw and Copy above apply to whatever is focused), rather than expanding a second set of controls.
function SecondaryList({ items, bodies, onSelect }: {
  items: ArtifactSummary[]
  bodies: Record<string, unknown>
  onSelect: (name: string) => void
}) {
  if (items.length === 0) return null
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg divide-y divide-gray-100 dark:divide-gray-700">
      {items.map(a => (
        <button
          key={a.name}
          type="button"
          onClick={() => onSelect(a.name)}
          className="w-full text-left px-3 py-2 min-h-[44px] medium:min-h-0 text-xs text-gray-700 dark:text-gray-200 hover:bg-gray-50 dark:hover:bg-gray-700/50"
        >
          {artifactTitle(a, bodies[a.name])}
        </button>
      ))}
    </div>
  )
}

// The monospace "Raw" fallback view: an exact line list, one judge-note
// highlight per line index. #1114 made ArtifactMarkdown/JsonView the DEFAULT
// - this stays reachable behind the Raw toggle for when the rendered view's own highlighting (data-line-based, see ArtifactMarkdown) doesn't anchor a note precisely enough.
function ArtifactLines({ lines, byLine, activeNote, onSelectNote }: {
  lines: string[]
  byLine: Map<number, JudgeNote[]>
  activeNote: JudgeNote | null
  onSelectNote: (n: JudgeNote) => void
}) {
  return (
    <pre className="text-xs font-mono bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg overflow-x-auto">
      <code>
        {lines.map((line, i) => {
          const notes = byLine.get(i) ?? []
          const highlighted = notes.length > 0
          return (
            <div
              key={i}
              className={`px-3 py-0.5 whitespace-pre-wrap break-words ${highlighted ? 'bg-amber-100 dark:bg-amber-900/30' : ''}`}
            >
              {line || ' '}
              {/* One button PER note, not one for the whole line - a line with
                  several notes (anchorNotes groups them) used to expose only
                  notes[0] to click/keyboard/screen readers; each is now its
                  own reachable, individually announced control. */}
              {notes.map((n, ni) => (
                <button
                  key={ni}
                  type="button"
                  aria-label={`Judge note on line ${i + 1}${notes.length > 1 ? ` (${ni + 1} of ${notes.length})` : ''}: ${n.text}`}
                  onClick={() => onSelectNote(n)}
                  className={`ml-1.5 inline-flex h-4 w-4 items-center justify-center rounded-full text-[11px] leading-none cursor-pointer ${
                    n === activeNote
                      ? 'bg-amber-400 dark:bg-amber-600 text-amber-950 dark:text-amber-50'
                      : 'bg-amber-200 dark:bg-amber-800 text-amber-800 dark:text-amber-200 hover:bg-amber-300 dark:hover:bg-amber-700'
                  }`}
                >
                  {ni + 1}
                </button>
              ))}
            </div>
          )
        })}
      </code>
    </pre>
  )
}

// DiffView colors unified-diff +/- lines - plain text otherwise, no library:
// the format is three characters of prefix per line, nothing to parse.
function DiffView({ text }: { text: string }) {
  const lines = text.split('\n')
  return (
    <pre className="text-xs font-mono bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg overflow-x-auto">
      <code>
        {lines.map((line, i) => {
          const color = line.startsWith('+') && !line.startsWith('+++')
            ? 'text-green-700 dark:text-green-400 bg-green-50 dark:bg-green-900/20'
            : line.startsWith('-') && !line.startsWith('---')
              ? 'text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/20'
              : line.startsWith('@@')
                ? 'text-blue-600 dark:text-blue-400'
                : ''
          return (
            <div key={i} className={`px-3 py-0.5 whitespace-pre-wrap break-words ${color}`}>
              {line || ' '}
            </div>
          )
        })}
      </code>
    </pre>
  )
}

// Answer text is markdown that may embed a little raw HTML (matches
// AgentParts' AssistantText schema exactly, for the same reason: model text
// there, judge/worker artifact text here).
const mdSchema = {
  ...defaultSchema,
  tagNames: [...(defaultSchema.tagNames ?? []), 'details', 'summary'],
}

// BLOCK_TAGS get a data-line attribute + judge-note highlight/click; picked
// to cover the elements a judge quote is actually likely to land inside.
const BLOCK_TAGS = ['p', 'li', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'td', 'th', 'blockquote'] as const

// HEADING_CLASS sizes headings explicitly rather than trusting the ambient
// `.prose h1` cascade (#1139 cosmetic follow-up: headings rendered at body
// size) - this tree sits under DagView's not-prose ancestor, and Tailwind Typography excludes EVERY descendant of a not-prose ancestor from ANY .prose styling, including a nested one.
const HEADING_CLASS: Record<string, string> = {
  h1: 'text-base font-bold mt-3 mb-1.5',
  h2: 'text-sm font-bold mt-3 mb-1.5',
  h3: 'text-sm font-semibold mt-2 mb-1',
  h4: 'text-xs font-semibold mt-2 mb-1',
  h5: 'text-xs font-semibold mt-2 mb-1',
  h6: 'text-xs font-semibold mt-2 mb-1 text-gray-500 dark:text-gray-400',
}

// Attaches a note to the block whose source range CONTAINS the anchor line,
// not just the block whose first line matches (#1139: a note anchored
// mid-paragraph used to render nowhere and wasn't listed as unanchored). blockquote is excluded: CommonMark nests a `> quote` as <blockquote><p>...</p></blockquote>, and wrapper + inner <p> share the same line range, so blockquote would double-render every note.
function notesInRange(byLine: Map<number, JudgeNote[]>, tag: string, node: Element | undefined): JudgeNote[] {
  if (tag === 'blockquote') return []
  const start = node?.position?.start.line
  const end = node?.position?.end?.line ?? start
  if (start == null) return []
  const notes: JudgeNote[] = []
  for (let line = start; line <= (end as number); line++) {
    const found = byLine.get(line - 1)
    if (found) notes.push(...found)
  }
  return notes
}

// Renders a blob through the same react-markdown pipeline AssistantText uses
// (headings, tables, code blocks with highlight + copy - #1114), keeping judge
// notes anchorable: remark/rehype keep each node's source line (node.position.start.line, 1-based, as elsewhere - see AgentParts' mermaid-detection `pre` override) - ponytail: stamping data-line via the existing `components` override achieves what a dedicated rehype plugin would, with no new plugin.
function ArtifactMarkdown({ text, byLine, activeNote, onSelectNote }: {
  text: string
  byLine: Map<number, JudgeNote[]>
  activeNote: JudgeNote | null
  onSelectNote: (n: JudgeNote) => void
}) {
  const components = useMemo(() => {
    function block(tag: string) {
      return function Block({ node, children, ...rest }: any) {
        const line = (node as Element | undefined)?.position?.start.line
        const notes = notesInRange(byLine, tag, node as Element | undefined)
        const highlighted = notes.length > 0
        return createElement(
          tag,
          {
            ...rest,
            'data-line': line,
            className: [HEADING_CLASS[tag], highlighted ? 'bg-amber-100 dark:bg-amber-900/30 rounded px-1 -mx-1' : ''].filter(Boolean).join(' ') || undefined,
          },
          children,
          notes?.map((n, ni) => (
            <button
              key={ni}
              type="button"
              aria-label={`Judge note on line ${line}${notes.length > 1 ? ` (${ni + 1} of ${notes.length})` : ''}: ${n.text}`}
              onClick={() => onSelectNote(n)}
              className={`not-prose ml-1.5 inline-flex h-4 w-4 items-center justify-center rounded-full text-[11px] leading-none cursor-pointer align-middle ${
                n === activeNote
                  ? 'bg-amber-400 dark:bg-amber-600 text-amber-950 dark:text-amber-50'
                  : 'bg-amber-200 dark:bg-amber-800 text-amber-800 dark:text-amber-200 hover:bg-amber-300 dark:hover:bg-amber-700'
              }`}
            >
              {ni + 1}
            </button>
          )),
        )
      }
    }
    const map: Record<string, any> = {}
    for (const tag of BLOCK_TAGS) map[tag] = block(tag)
    map.pre = CopyablePre
    return map
  }, [byLine, activeNote, onSelectNote])

  // escapeUnmatchedBackticks (#746) only inserts a `\` before an isolated
  // backtick within its line - it never adds/removes a newline, so line
  // numbers (what byLine/data-line anchor on) are unaffected; only within-line offsets shift, which nothing here reads.
  const fixed = useMemo(() => escapeUnmatchedBackticks(text), [text])

  return (
    <div className="prose prose-sm dark:prose-invert max-w-none break-words bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-4 py-3">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlightSubset]}
        components={components}
      >{fixed}</ReactMarkdown>
    </div>
  )
}

// typedValue renders one JSON leaf value, colored by type - the same palette
// DiffView/JudgeCard already use elsewhere in this file/AgentParts, so a
// number/string/boolean reads the same way across the app, not just here.
function typedValue(v: unknown) {
  if (v === null) return <span className="text-gray-500 dark:text-gray-400 italic">null</span>
  if (typeof v === 'string') return <span className="text-green-700 dark:text-green-400 break-words">"{v}"</span>
  if (typeof v === 'number') return <span className="text-blue-600 dark:text-blue-400">{v}</span>
  if (typeof v === 'boolean') return <span className="text-purple-600 dark:text-purple-400">{String(v)}</span>
  return <span className="text-gray-800 dark:text-gray-100 break-words">{JSON.stringify(v)}</span>
}

// JsonNode renders one key/value pair of a JsonTree - a nested object/array
// gets its own <details> (native disclosure, no JS state needed per node);
// a leaf renders inline via typedValue.
function JsonNode({ k, v }: { k?: string; v: unknown }) {
  const isContainer = v !== null && typeof v === 'object'
  if (!isContainer) {
    return (
      <div className="py-0.5 text-xs">
        {k != null && <span className="text-gray-500 dark:text-gray-400">{k}: </span>}
        {typedValue(v)}
      </div>
    )
  }
  const isArray = Array.isArray(v)
  const entries: [string, unknown][] = isArray
    ? (v as unknown[]).map((item, i) => [String(i), item])
    : Object.entries(v as Record<string, unknown>)
  return (
    <details open className="text-xs">
      <summary className="cursor-pointer select-none py-0.5">
        {k != null && <span className="text-gray-500 dark:text-gray-400">{k}: </span>}
        <span className="text-gray-500 dark:text-gray-400">
          {isArray ? `Array(${entries.length})` : `Object{${entries.length}}`}
        </span>
      </summary>
      <div className="ml-3 pl-2 border-l border-gray-200 dark:border-gray-700">
        {entries.length === 0 && <div className="py-0.5 text-gray-500 dark:text-gray-400 italic">empty</div>}
        {entries.map(([ck, cv]) => <JsonNode key={ck} k={ck} v={cv} />)}
      </div>
    </details>
  )
}

// JsonView is the collapsible key/value tree default view - for an unknown
// structured kind only now that the known kinds have their own typed view
// below; the pretty-printed code block stays behind the "Raw" toggle, ArtifactLines.
function JsonView({ data }: { data: unknown }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2 overflow-x-auto">
      <JsonNode v={data} />
    </div>
  )
}

// severityChip colors a finding/review severity word the same way across
// FindingView, ReviewView's inline findings, and the review's own verdict.
const SEVERITY_COLOR: Record<string, string> = {
  blocking: 'text-red-600 dark:text-red-400',
  request_changes: 'text-red-600 dark:text-red-400',
  suggestion: 'text-amber-600 dark:text-amber-400',
  nit: 'text-gray-500 dark:text-gray-400',
  approve: 'text-green-700 dark:text-green-400',
}
function severityChip(word: string | undefined) {
  if (!word) return null
  return <span className={`font-medium ${SEVERITY_COLOR[word] ?? 'text-gray-600 dark:text-gray-300'}`}>{word}</span>
}

// FindingView: severity chip, path:line, title, rationale, snippet as code.
function FindingView({ data }: { data: FindingBody }) {
  const loc = findingLoc(data)
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2 space-y-1.5 text-xs">
      <div className="flex flex-wrap items-center gap-1.5">
        {severityChip(data.severity)}
        {loc && <span className="font-mono text-gray-500 dark:text-gray-400">{loc}</span>}
      </div>
      {data.title && <p className="font-medium text-gray-800 dark:text-gray-100">{data.title}</p>}
      {data.rationale && <p className="text-gray-600 dark:text-gray-300">{data.rationale}</p>}
      {data.snippet && <pre className="font-mono text-[11px] bg-gray-50 dark:bg-gray-900 border border-gray-200 dark:border-gray-700 rounded px-2 py-1 overflow-x-auto whitespace-pre-wrap break-words">{data.snippet}</pre>}
    </div>
  )
}

// ReviewView: verdict chip, takeaway, the verified/notes lists, and its
// findings inline in finding_ids order (resolved by the caller from
// secondaryBodies - a review doesn't carry finding bodies itself, only their ids).
function ReviewView({ data, findings }: { data: CodeReviewBody; findings: { id: string; body: FindingBody | undefined }[] }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2 space-y-2 text-xs">
      <div className="flex flex-wrap items-center gap-1.5">
        {severityChip(data.verdict)}
        {data.takeaway && <span className="text-gray-700 dark:text-gray-200">{data.takeaway}</span>}
      </div>
      <MetaList label="Verified" items={data.verified} />
      <MetaList label="Notes" items={data.notes} />
      {findings.length > 0 && (
        <div className="space-y-1.5">
          <span className="text-[11px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">Findings</span>
          {findings.map(f => <FindingView key={f.id} data={f.body ?? {}} />)}
        </div>
      )}
    </div>
  )
}

function MetaList({ label, items }: { label: string; items: string[] | undefined }) {
  if (!items || items.length === 0) return null
  return (
    <div>
      <span className="text-[11px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">{label}</span>
      <ul className="mt-0.5 list-disc list-inside space-y-0.5 text-gray-600 dark:text-gray-300">
        {items.map((it, i) => <li key={i}>{it}</li>)}
      </ul>
    </div>
  )
}

// JudgeRoundView: score, pass/fail, the criteria table, probes.
// Per-criterion scores are a raw 0-3 number, not a fraction (#941 scaleSpec) - shown as-is, same as the old JsonView summary did.
function JudgeRoundView({ data }: { data: JudgeRoundContent }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2 space-y-2 text-xs">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className={`inline-flex items-center gap-1 font-medium ${data.passed ? 'text-green-700 dark:text-green-400' : 'text-red-600 dark:text-red-400'}`}>
          <Icon name={data.passed ? 'check' : 'close'} className="w-3.5 h-3.5" /> {data.passed ? 'passed' : 'failed'}
        </span>
        {data.score != null && <span className="text-gray-500 dark:text-gray-400 tabular-nums">{data.score.toFixed(2)}</span>}
      </div>
      {data.criteria && data.criteria.length > 0 && (
        <table className="w-full text-left">
          <tbody>
            {data.criteria.map((c, i) => (
              <tr key={i} className="border-t border-gray-100 dark:border-gray-700 first:border-t-0">
                <td className="py-1 pr-2 font-medium text-gray-700 dark:text-gray-200 align-top whitespace-nowrap">{c.name}</td>
                <td className="py-1 pr-2 text-gray-500 dark:text-gray-400 align-top tabular-nums">{c.score}</td>
                <td className="py-1 text-gray-600 dark:text-gray-300 align-top">{c.feedback}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {data.evidence?.probes && data.evidence.probes.length > 0 && (
        <div>
          <span className="text-[11px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">Probes</span>
          <ul className="mt-0.5 space-y-0.5 text-gray-600 dark:text-gray-300">
            {data.evidence.probes.map((p, i) => <li key={i}>{p.name}: {p.result}</li>)}
          </ul>
        </div>
      )}
    </div>
  )
}

// PlanView: the assignment list. dag_plan is bookkeeping excluded from a
// node's own panel, so this renders only via a direct fixture/test, not a reachable node-panel state today.
export function PlanView({ data }: { data: PlanBody }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2 space-y-1.5 text-xs">
      {data.status && <p className="text-gray-500 dark:text-gray-400">status: {data.status}</p>}
      {(data.assignments ?? []).map((a, i) => (
        <div key={i} className="border-t border-gray-100 dark:border-gray-700 first:border-t-0 pt-1.5">
          <p className="font-medium text-gray-800 dark:text-gray-100">{a.node_id}</p>
          {a.task && <p className="text-gray-600 dark:text-gray-300 line-clamp-3 whitespace-pre-wrap">{a.task}</p>}
        </div>
      ))}
    </div>
  )
}

// typedView dispatches a structured artifact's known kind to its own view;
// undefined for anything else, so the caller falls back to the generic JSON tree - unknown kinds keep the tree.
function typedView(kind: string | undefined, data: unknown, reviewFindings: { id: string; body: FindingBody | undefined }[]): ReactNode | undefined {
  switch (kind) {
    case 'code_review': return <ReviewView data={data as CodeReviewBody} findings={reviewFindings} />
    case 'finding': return <FindingView data={data as FindingBody} />
    case 'judge_round': return <JudgeRoundView data={data as JudgeRoundContent} />
    case 'dag_plan': return <PlanView data={data as PlanBody} />
    default: return undefined
  }
}

// The primary output and a focused secondary share ONE renderer stack - the
// Raw line list, a typed view per known kind (JSON tree for anything else),
// and rendered markdown - so both surfaces can't drift as kinds grow.
function ArtifactView({ content, displayText, lines, rawView, isStructured, parsedJson, kind, byLine, activeNote, onSelectNote, reviewFindings }: {
  content: string | null
  displayText: string | null
  lines: string[]
  rawView: boolean
  isStructured: boolean
  parsedJson: unknown
  kind: string | undefined
  byLine: Map<number, JudgeNote[]>
  activeNote: JudgeNote | null
  onSelectNote: (n: JudgeNote) => void
  reviewFindings?: { id: string; body: FindingBody | undefined }[]
}) {
  if (displayText == null) return <p className="text-xs text-gray-500 dark:text-gray-400">Loading…</p>
  if (rawView) return <ArtifactLines lines={lines} byLine={byLine} activeNote={activeNote} onSelectNote={onSelectNote} />
  if (!isStructured) return <ArtifactMarkdown text={content ?? ''} byLine={byLine} activeNote={activeNote} onSelectNote={onSelectNote} />
  if (parsedJson === undefined) return <ArtifactLines lines={lines} byLine={byLine} activeNote={activeNote} onSelectNote={onSelectNote} />
  const typed = typedView(kind, parsedJson, reviewFindings ?? [])
  return typed ?? <JsonView data={parsedJson} />
}

// One tapped judge note's callout under the rendered output.
function ActiveNoteCallout({ note }: { note: JudgeNote | null }) {
  if (!note) return null
  return (
    <div className="rounded-lg border border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-900/20 px-3 py-2 text-xs text-amber-800 dark:text-amber-300">
      {note.criterion && <span className="font-semibold mr-1">{note.criterion}:</span>}
      {note.text}
    </div>
  )
}

// One chip of the judge-round timeline: a button, never a picker - tapping
// one shows that round's notes on the revision it judged.
function RoundChip({ id, round, passed, score, active, onActivate }: {
  id: string
  round: number | undefined
  passed: boolean | undefined
  score: number | undefined
  active: boolean
  onActivate: () => void
}) {
  const verdict = passed == null ? 'no verdict' : passed ? 'passed' : 'failed'
  return (
    <button
      key={id}
      type="button"
      aria-pressed={active}
      aria-label={`Round ${round}, ${verdict}${score != null ? `, score ${Math.round(score * 100)}%` : ''}`}
      onClick={onActivate}
      className={`shrink-0 inline-flex items-center gap-1 h-11 medium:h-8 px-3 rounded-full border text-xs transition-colors ${
        active
          ? 'border-blue-400 dark:border-blue-500 bg-blue-50 dark:bg-blue-900/30'
          : 'border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 hover:bg-gray-100 dark:hover:bg-gray-700'
      }`}
    >
      <span className="font-medium text-gray-700 dark:text-gray-200">Round {round}</span>
      <span aria-hidden="true" className="text-gray-500 dark:text-gray-400">·</span>
      <span className={passed == null ? 'text-gray-500 dark:text-gray-400' : passed ? 'text-green-700 dark:text-green-400' : 'text-red-600 dark:text-red-400'}>
        {verdict}
      </span>
      {score != null && (
        <>
          <span aria-hidden="true" className="text-gray-500 dark:text-gray-400">·</span>
          <span className="text-gray-600 dark:text-gray-300 tabular-nums">{Math.round(score * 100)}%</span>
        </>
      )}
    </button>
  )
}

// The judge-round timeline: pinned directly under the header, non-scrolling
// block, horizontal scroll when the rounds overflow 390px. Chips are buttons,
// never a picker: tapping one shows that round's notes on the revision it judged.
function TimelineBlock({ chips, hidden, activeRoundId, onActivate }: {
  chips: { id: string; b: JudgeRoundContent }[]
  hidden: boolean
  activeRoundId: string | null
  onActivate: (id: string) => void
}) {
  if (hidden || chips.length === 0) return null
  return (
    <div
      role="group"
      aria-label="Judge rounds"
      className="shrink-0 flex items-center gap-1.5 overflow-x-auto px-4 py-1.5 border-b border-gray-200 dark:border-gray-700"
    >
      {chips.map(({ id, b }) => (
        <RoundChip key={id} id={id} round={b.round} passed={b.passed} score={b.score} active={activeRoundId === id} onActivate={() => onActivate(id)} />
      ))}
    </div>
  )
}

// The fetch-error line above the output - a content/diff failure shares it.
function ErrorLine({ error }: { error: string | null }) {
  if (!error) return null
  return <p className="text-xs text-red-500 dark:text-red-400">{error}</p>
}

// The primary output's view slot: the diff (when active with a loaded body)
// or the shared renderer stack.
function PrimaryView({ diffActive, diffText, content, displayText, lines, rawView, isStructured, parsedJson, kind, byLine, activeNote, onSelectNote, reviewFindings }: {
  diffActive: boolean
  diffText: string | null
  content: string | null
  displayText: string | null
  lines: string[]
  rawView: boolean
  isStructured: boolean
  parsedJson: unknown
  kind: string | undefined
  byLine: Map<number, JudgeNote[]>
  activeNote: JudgeNote | null
  onSelectNote: (n: JudgeNote) => void
  reviewFindings: { id: string; body: FindingBody | undefined }[]
}) {
  if (diffActive && diffText != null) return <DiffView text={diffText} />
  return (
    <ArtifactView
      content={content}
      displayText={displayText}
      lines={lines}
      rawView={rawView && !diffActive}
      isStructured={isStructured}
      parsedJson={parsedJson}
      kind={kind}
      byLine={byLine}
      activeNote={activeNote}
      onSelectNote={onSelectNote}
      reviewFindings={reviewFindings}
    />
  )
}

// The "Unanchored notes" list - judge notes that anchor to no line of the
// shown revision (design V4 §9).
function UnanchoredNotes({ notes }: { notes: JudgeNote[] }) {
  if (notes.length === 0) return null
  return (
    <div>
      <span className="text-[11px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">
        Unanchored notes
      </span>
      <ul className="mt-1 space-y-1">
        {notes.map((n, i) => (
          <li key={i} className="text-xs text-gray-500 dark:text-gray-400">
            {n.criterion && <span className="font-semibold mr-1">{n.criterion}:</span>}
            {n.text}
          </li>
        ))}
      </ul>
    </div>
  )
}

// Details: the one place a raw id may appear (#1178). Its content renders
// only while OPEN - a closed native <details> keeps content in the DOM
// (readable by text-scanning tools), and the raw id must exist NOWHERE until opened.
function DetailsSection({ curInfo, open, onOpenChange, nodeTask, primary, revisionCount, onTrigger, chatId }: {
  curInfo: ArtifactRevisionInfo | null
  open: boolean
  onOpenChange: (open: boolean) => void
  nodeTask: string
  primary: ArtifactSummary
  revisionCount: number
  onTrigger?: () => void
  chatId: string
}) {
  if (curInfo == null) return null
  return (
    <details
      onToggle={e => onOpenChange(e.currentTarget.open)}
      className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg"
    >
      <summary className="cursor-pointer select-none px-3 py-2 text-[11px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">
        Details
      </summary>
      {open && (
        <div className="px-3 pb-3">
          <MetaRow label="task">
            <p className="line-clamp-6 whitespace-pre-wrap">{nodeTask}</p>
          </MetaRow>
          <ArtifactMetadata
            summary={primary}
            revision={curInfo}
            revisionCount={revisionCount}
            onTrigger={onTrigger}
            url={artifactUrl(chatId, primary.name, curInfo.revision)}
          />
        </div>
      )}
    </details>
  )
}

// The primary output's Revision N-of-M bar: prev/next cursor, the single
// Diff toggle (disabled with its visible reason), the Raw fallback toggle,
// and copy.
function RevisionBar({ currentRev, count, revIdx, diffActive, diffDisabledReason, onPrev, onNext, onToggleDiff, rawView, onToggleRaw, displayText }: {
  currentRev: number | null
  count: number
  revIdx: number | null
  diffActive: boolean
  diffDisabledReason: string | null
  onPrev: () => void
  onNext: () => void
  onToggleDiff: () => void
  rawView: boolean
  onToggleRaw: () => void
  displayText: string | null
}) {
  if (count === 0) return null
  return (
    <div className="flex items-center gap-1.5 flex-wrap text-xs">
      <span aria-live="polite" className="text-gray-600 dark:text-gray-300 tabular-nums">
        Revision {currentRev ?? '–'} of {count}
      </span>
      <button
        onClick={onPrev}
        aria-label="Previous revision"
        disabled={revIdx == null || revIdx <= 0}
        className="inline-flex h-11 medium:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
      >
        ← Prev
      </button>
      <button
        onClick={onNext}
        aria-label="Next revision"
        disabled={revIdx == null || revIdx >= count - 1}
        className="inline-flex h-11 medium:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
      >
        Next →
      </button>
      <button
        onClick={onToggleDiff}
        aria-pressed={diffActive}
        disabled={diffDisabledReason != null}
        title={diffDisabledReason ?? undefined}
        className="inline-flex h-11 medium:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
      >
        Diff
      </button>
      {diffDisabledReason != null && (
        <span className="text-gray-500 dark:text-gray-400">{diffDisabledReason}</span>
      )}
      <button
        onClick={onToggleRaw}
        aria-pressed={rawView && !diffActive}
        disabled={diffActive}
        className="inline-flex h-11 medium:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
      >
        Raw
      </button>
      {displayText != null && <CopyButton text={displayText} label="Copy artifact text" />}
    </div>
  )
}

// The panel's empty state: the failure message when the node errored before
// writing a non-judge artifact; otherwise names the delivery (e.g. an ACP
// implementer's own answer text, "Opened PR #1464…") instead of the old generic "hasn't produced anything yet".
function EmptyState({ nodeError, deliveryText }: { nodeError?: string; deliveryText: string | null }) {
  return (
    <div className="flex flex-col items-center justify-center gap-1 min-h-[14rem] text-center px-6">
      {nodeError ? (
        <>
          <p className="text-sm font-medium text-gray-700 dark:text-gray-200">This node failed before writing its result.</p>
          <p className="text-xs text-red-600 dark:text-red-400 break-words">{nodeError}</p>
        </>
      ) : deliveryText ? (
        <p className="text-sm text-gray-500 dark:text-gray-400 break-words">{deliveryText}</p>
      ) : (
        <p className="text-sm text-gray-500 dark:text-gray-400">This node hasn't produced anything yet.</p>
      )}
    </div>
  )
}

// fmtRelative renders "3m ago"/"in 2h" etc via the native
// Intl.RelativeTimeFormat - no date library for one small formatter
// (ponytail).
function fmtRelative(iso: string): string {
  const diffMs = new Date(iso).getTime() - Date.now()
  const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })
  const abs = Math.abs(diffMs)
  const units: [number, Intl.RelativeTimeFormatUnit][] = [
    [1000 * 60 * 60 * 24, 'day'], [1000 * 60 * 60, 'hour'], [1000 * 60, 'minute'], [1000, 'second'],
  ]
  for (const [ms, unit] of units) {
    if (abs >= ms || unit === 'second') return rtf.format(Math.round(diffMs / ms), unit)
  }
  return rtf.format(0, 'second')
}

// Renders "Jun 21, 14:32" INLINE next to the relative time (#1139: a
// hover-only `title` tooltip is unreachable on a touch device, and this
// panel's mobile pass makes touch the primary surface). Full ISO still lives in `title` for a pointer user.
function fmtAbsoluteShort(iso: string): string {
  return new Date(iso).toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

// MetaRow is one key/value line, same visual language as JsonNode's leaves -
// the metadata block is "the JSON tree's styling" applied to a fixed set of
// fields (owner request on #1114) rather than a second, differently-styled table.
function MetaRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="py-0.5 text-xs flex gap-1">
      <span className="text-gray-500 dark:text-gray-400 shrink-0">{label}:</span>
      <span className="text-gray-800 dark:text-gray-100 break-words min-w-0">{children}</span>
    </div>
  )
}

// The Details' lineage block (node, round, parent revision, trigger, head
// sha) - the one row that jumps to its chip: trigger, and only when that
// round is one of this node's (onTrigger); plain text otherwise.
function LineageRows({ l, onTrigger }: { l: NonNullable<ArtifactRevisionInfo['lineage']>; onTrigger?: () => void }) {
  return (
    <>
      {l.node_id && <MetaRow label="node">{l.node_id}</MetaRow>}
      {l.round != null && <MetaRow label="round">{l.round}</MetaRow>}
      {l.parent_revision != null && <MetaRow label="parent revision">{l.parent_revision}</MetaRow>}
      {l.trigger_annotation && (
        <MetaRow label="trigger">
          {onTrigger ? (
            <button
              type="button"
              onClick={onTrigger}
              className="text-blue-600 dark:text-blue-400 hover:underline break-words text-left"
            >
              {l.trigger_annotation}
            </button>
          ) : (
            l.trigger_annotation
          )}
        </MetaRow>
      )}
      {l.head_sha && (
        <MetaRow label="head sha">
          <span className="inline-flex items-center gap-1">
            <span className="font-mono">{l.head_sha.slice(0, 12)}</span>
            <CopyButton text={l.head_sha} label="Copy head sha" />
          </span>
        </MetaRow>
      )}
    </>
  )
}

// The panel's Details disclosure content (#1178): the one place the raw id,
// kind, class, per-revision lineage, timestamps and the REST link appear.
// The trigger row (the judge_round that produced this revision) jumps to that round's chip when it's one of this node's; plain text otherwise.
function ArtifactMetadata({ summary, revision, revisionCount, onTrigger, url }: {
  summary: ArtifactSummary
  revision: ArtifactRevisionInfo
  revisionCount: number
  onTrigger?: () => void
  url: string
}) {
  const l = revision.lineage
  return (
    <div className="text-xs">
      <MetaRow label="id">{summary.name}</MetaRow>
      {summary.kind && <MetaRow label="kind">{summary.kind}</MetaRow>}
      {summary.class && <MetaRow label="class">{summary.class}</MetaRow>}
      <MetaRow label="mime">{revision.mime_type}</MetaRow>
      <MetaRow label="size">{revision.size.toLocaleString()} bytes</MetaRow>
      <MetaRow label="revision">{revision.revision} of {revisionCount}</MetaRow>
      {l && <LineageRows l={l} onTrigger={onTrigger} />}
      {revision.turn_id && <MetaRow label="turn">{revision.turn_id}</MetaRow>}
      {/* author covers "dispatch source" for an input too - dispatch is
          already the value lineage.author carries for one; there's no
          separate source field in the schema to show beyond it. */}
      {l?.author && <MetaRow label="author">{l.author}</MetaRow>}
      {revision.created_at && (
        <MetaRow label="saved">
          <span title={revision.created_at}>
            {fmtRelative(revision.created_at)} · {fmtAbsoluteShort(revision.created_at)}
          </span>
        </MetaRow>
      )}
      <MetaRow label="REST">
        <a
          href={url}
          target="_blank"
          rel="noreferrer"
          className="text-blue-600 dark:text-blue-400 hover:underline break-all"
        >
          {/* Decoded for display: the href is the encoded URL, but
              text%3Aplan is a machine's reading of text:plan. */}
          {decodeURIComponent(url)}
        </a>
      </MetaRow>
    </div>
  )
}
