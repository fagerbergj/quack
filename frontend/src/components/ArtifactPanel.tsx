import { createElement, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeRaw from 'rehype-raw'
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize'
import rehypeHighlight from 'rehype-highlight'
import 'highlight.js/styles/github-dark.css'
import type { Element } from 'hast'
import { api, artifactUrl } from '../api'
import type { ArtifactSummary, ArtifactRevisionInfo } from '../api'
import { CopyButton } from './CopyButton'
import { CopyablePre } from './CopyablePre'
import { escapeUnmatchedBackticks } from '../lib/backticks'
import { useChatStore } from '../state/ChatStoreProvider'

// JudgeRoundContent is the JSON body of a `judge_round` artifact (design V4
// §4.3) - the only place a note's line anchor lives. Fetched and parsed
// client-side; not part of the generated schema since it's an artifact body,
// not a REST response shape.
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
  round?: number
  passed?: boolean
  score?: number
  // The artifact revisions this round judged (server shape:
  // internal/vetting/reviewrecord.go's ScoredRef, json "scored"). A round
  // can score several artifacts; the timeline uses the ref that points at
  // the node's primary output.
  scored?: { artifact_id: string; revision: number }[]
  notes?: JudgeNote[]
}

interface AnchorResult {
  byLine: Map<number, JudgeNote[]>
  unanchored: JudgeNote[]
}

// anchorNotes locates each note in the shown revision's text: an exact
// substring match on its quoted snippet wins; line_hint is the fallback when
// no line contains the snippet (a line shift, or a stale snippet after an
// edit); a note that matches neither is unanchored (design V4 §9).
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

// selectPrimaryOutput is the node's RESULT: the non-judge artifact the panel
// opens onto, computed rather than chosen (#1178 - there is no picker). The
// node's declared output kind (DagNodeDef.artifact) wins when the node has
// an artifact of that kind; otherwise the newest output - highest
// latest_revision, then lineage saved_at, then name as the final tiebreak.
export function selectPrimaryOutput(artifacts: ArtifactSummary[], nodeArtifactKind?: string): ArtifactSummary | null {
  const nonJudge = artifacts.filter(a => a.kind !== 'judge_round')
  if (nonJudge.length === 0) return null
  const declared = nodeArtifactKind ? nonJudge.find(a => a.kind === nodeArtifactKind) : undefined
  if (declared) return declared
  return nonJudge.reduce((best, a) => (compareOutput(a, best) > 0 ? a : best))
}

function compareOutput(a: ArtifactSummary, b: ArtifactSummary): number {
  // Positive when `a` beats `b`: newer output first (latest_revision), then
  // later-saved lineage, then the alphabetically EARLIER name as the final
  // deterministic tiebreak.
  const ar = a.latest_revision ?? 0
  const br = b.latest_revision ?? 0
  if (ar !== br) return ar - br
  const as = a.lineage?.saved_at ?? ''
  const bs = b.lineage?.saved_at ?? ''
  if (as !== bs) return as > bs ? 1 : -1
  return a.name > b.name ? -1 : a.name < b.name ? 1 : 0
}

// resolveScoredRevision is the revision a judge round judged for the given
// artifact - from the round's scored list (keyed by artifact id, since a
// round can score several) - falling back to the artifact's latest revision
// when the round didn't score it.
export function resolveScoredRevision(body: JudgeRoundContent, primaryId: string, fallback: number): number {
  return body.scored?.find(s => s.artifact_id === primaryId)?.revision ?? fallback
}

// toAscending converts the revisions endpoint's newest-first ordering
// (openapi.yaml's ArtifactRevisionList) into the ascending list the panel's
// numeric cursor walks through.
export function toAscending(revs: ArtifactRevisionInfo[]): ArtifactRevisionInfo[] {
  return [...revs].reverse()
}

// isDiffableMime mirrors the diff endpoint's allowlist
// (internal/server/rest/artifacts.go: only application/json and text/* -
// anything else 415s), so the toggle can say why it's disabled BEFORE the
// request instead of after the error.
function isDiffableMime(mime: string): boolean {
  return mime === 'application/json' || mime.startsWith('text/')
}

// humanKindLabel is the "More" section's display name for a kind - the
// panel never shows raw ids/kinds outside the Details disclosure (#1178),
// so a kind the mapping doesn't name is at least capitalized.
export function humanKindLabel(kind?: string): string {
  switch (kind) {
    case 'finding': return 'Findings'
    case 'code_review': return 'Review'
    case 'pr_body': return 'PR description'
    case 'document': return 'Document'
    case 'text':
    case 'bytes': return 'Files'
    default: return kind ? kind.charAt(0).toUpperCase() + kind.slice(1) : 'Other'
  }
}

export interface SecondaryGroup {
  label: string
  items: ArtifactSummary[]
}

// groupSecondary buckets the node's secondary artifacts (everything but
// the primary output and the judge rounds) into the "More" section's
// labelled groups; first-seen order, one group per display label.
export function groupSecondary(items: ArtifactSummary[]): SecondaryGroup[] {
  const groups = new Map<string, ArtifactSummary[]>()
  for (const a of items) {
    const label = humanKindLabel(a.kind)
    const g = groups.get(label) ?? []
    g.push(a)
    groups.set(label, g)
  }
  return [...groups.entries()].map(([label, items]) => ({ label, items }))
}

// moreItemLabel is a "More" row's label: the instance half of the id (its
// hint - human for text/bytes/…), never the full kind:instance id. A
// finding's instance is a content hash, so it gets an ordinal instead.
function moreItemLabel(a: ArtifactSummary, ordinal: number): string {
  const i = a.name.indexOf(':')
  const instance = i >= 0 ? a.name.slice(i + 1) : a.name
  if (a.kind === 'finding' || instance === '') return `#${ordinal}`
  return instance
}

interface Props {
  chatId: string
  nodeId: string
  // The node's display name - the panel's <h2>. Same agentLabel() DagNode's
  // own card header shows (#1216 review): the raw prompt (nodeTask) can run
  // to a kilobyte, which blew the header past the sheet height when it was
  // shown here instead - it now lives only in the Details "Task" row.
  nodeAgent: string
  // The node's raw prompt - shown in Details, never the header (see above).
  nodeTask: string
  // The node's error text (NodeState.error). Drives the failure empty state
  // when the node has no non-judge artifact.
  nodeError?: string
  // The node's declared output kind (DagNodeDef.artifact) - makes the
  // primary-output selection exact. Absent when the node declares none.
  nodeArtifactKind?: string
  onClose: () => void
}

// A result view, not a picker (#1178): opening the panel shows the node's
// primary output rendered under the node's own name, with the judge rounds
// as a chip timeline above it, a Revision N-of-M prev/next bar with a
// single diff toggle, secondary artifacts behind "More", and everything
// provenance-shaped (id, kind, class, lineage, timestamps, the REST link)
// in a collapsed "Details" disclosure at the very bottom. Both native
// <select>s and the desktop sidebar from the picker era are gone - there is
// nothing left to pick.
export function ArtifactPanel({ chatId, nodeId, nodeAgent, nodeTask, nodeError, nodeArtifactKind, onClose }: Props) {
  const [summaries, setSummaries] = useState<ArtifactSummary[]>([])
  const [error, setError] = useState<string | null>(null)
  // rawView: the monospace line-list is the FALLBACK view (also what a
  // judge-note highlight needs a stable line index for - #1114 keeps it as
  // a toggle rather than dropping it), not the default; structured kinds
  // default to the collapsible tree, blobs to rendered markdown.
  const [rawView, setRawView] = useState(false)
  // activeRoundId is the tapped timeline chip (null = no chip active, every
  // round's matching notes anchor). Declared up here with the other state;
  // the activation effect below is what it drives.
  const [activeRoundId, setActiveRoundId] = useState<string | null>(null)

  const load = useCallback(() => {
    // Returns the promise (not fire-and-forget) - withScrollPreserved below
    // awaits it to restore scroll only after the refetch actually lands.
    return api.listChatArtifacts(chatId).then(l => setSummaries(l.data ?? [])).catch(e => setError(String(e)))
  }, [chatId])
  useEffect(() => { load() }, [load])

  // Membership is by the LATEST revision's lineage.node_id (ArtifactSummary
  // only carries that revision's lineage - see toArtifactSummary in
  // internal/server/rest/artifacts.go), not "any revision this node wrote".
  // If a later node revises an artifact (e.g. a judge writes revision 2 of
  // a worker's `finding`), it moves to the reviser's panel and disappears
  // from the original author's - a real gap against a "everything this node
  // wrote is an output" reading of design V4, open as a question on #1094's
  // review pending a spec answer. Fixing it needs per-revision lineage
  // (GET .../revisions) fetched for every artifact in the chat up front,
  // which doesn't scale to "one click opens a panel" - documented here
  // rather than silently wrong.
  const nodeArtifacts = useMemo(
    () => summaries.filter(s => s.lineage?.node_id === nodeId),
    [summaries, nodeId],
  )

  // The panel's one and only artifact: computed, not chosen. Judge rounds
  // are not candidates - they are the timeline above the output, not
  // pickable content.
  const primary = useMemo(() => selectPrimaryOutput(nodeArtifacts, nodeArtifactKind), [nodeArtifacts, nodeArtifactKind])
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
  useEffect(() => { loadJudgeBodies() }, [loadJudgeBodies])

  // The timeline: one chip per parsed judge body, round order.
  const chips = useMemo(() => {
    const entries = Object.entries(judgeBodies).map(([id, b]) => ({ id, b }))
    entries.sort((x, y) => (x.b.round ?? 0) - (y.b.round ?? 0))
    return entries
  }, [judgeBodies])

  // Revisions of the primary, ascending (the endpoint returns
  // newest-first) - the cursor indexes this list.
  const [revisions, setRevisions] = useState<ArtifactRevisionInfo[]>([])
  const [revIdx, setRevIdx] = useState<number | null>(null)
  const curInfo = revIdx != null ? revisions[revIdx] ?? null : null
  const currentRev = curInfo?.revision ?? null
  const latestRev = revisions.length > 0 ? revisions[revisions.length - 1]?.revision ?? null : primary?.latest_revision ?? null

  const revisionsToken = useRef(0)
  // Read via a ref (not a useCallback dep) so a cursor MOVE never
  // re-fetches the list: refresh() calls this directly and wants the
  // cursor as it is NOW, while the effect only re-fires on a new primary.
  const currentRevRef = useRef(currentRev)
  currentRevRef.current = currentRev
  const loadRevisions = useCallback(() => {
    if (!primaryId) { setRevisions([]); setRevIdx(null); return }
    const token = ++revisionsToken.current
    return api.listArtifactRevisions(chatId, primaryId)
      .then(r => {
        if (token !== revisionsToken.current) return
        setError(null)
        const asc = toAscending(r.data ?? [])
        setRevisions(asc)
        // Keep the currently viewed revision across a refresh if it still
        // exists; a fresh primary or a vanished revision lands on latest.
        const want = currentRevRef.current
        const wantIdx = want != null ? asc.findIndex(x => x.revision === want) : -1
        setRevIdx(wantIdx >= 0 ? wantIdx : asc.length - 1)
      })
      .catch(e => { if (token === revisionsToken.current) setError(String(e)) })
  }, [chatId, primaryId])
  useEffect(() => { loadRevisions() }, [loadRevisions])

  // The tapped chip's judged revision, as an effect (not the click
  // handler): a tap can land BEFORE the revision list has arrived (both
  // fetches start together on open), and this applies it as soon as the
  // list does. move() clears activeRoundId before the cursor changes, so
  // this never fights a manual prev/next.
  const activeBody = activeRoundId != null ? judgeBodies[activeRoundId] : null
  useEffect(() => {
    if (activeRoundId == null || primaryId == null || revisions.length === 0) return
    const b = judgeBodies[activeRoundId]
    if (!b) return
    const rev = resolveScoredRevision(b, primaryId, latestRev ?? revisions[revisions.length - 1].revision)
    const idx = revisions.findIndex(x => x.revision === rev)
    const target = idx >= 0 ? idx : revisions.length - 1
    if (target !== revIdx) setRevIdx(target)
  }, [activeRoundId, judgeBodies, revisions])

  // Content of the cursor's revision. A chip tap that targets an as-yet
  // unloaded revision reaches this same path: revIdx moves, this fires the
  // getArtifactText fetch, and the token ref below keeps a slow prior
  // response from clobbering it (the stale-response guard the picker era
  // built is kept - artifact switches are gone, refreshes remain).
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
  useEffect(() => { loadContent() }, [loadContent])

  // Diff, always against the PREVIOUS revision (there is no "against"
  // picker): one toggle. Disabled with a visible reason when there is no
  // previous revision, either side is a binary mime (endpoint 415 -
  // checked here against the revision list's own mime_type, mirroring
  // the server's allowlist), or the server has rejected this pair as
  // too large (413 over the 256KB bound).
  const [diffOn, setDiffOn] = useState(false)
  const [diffBlocked, setDiffBlocked] = useState<string | null>(null)
  const [diffFailed, setDiffFailed] = useState(false)
  const [diffText, setDiffText] = useState<string | null>(null)
  const prevInfo = revIdx != null && revIdx > 0 ? revisions[revIdx - 1] ?? null : null
  const diffDisabledReason =
    curInfo == null
      ? 'No revision to diff'
      : prevInfo == null
        ? 'No previous revision'
        : !isDiffableMime(curInfo.mime_type) || !isDiffableMime(prevInfo.mime_type)
          ? 'Only text and JSON revisions can be diffed'
          : diffBlocked
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
  useEffect(() => { loadDiff() }, [loadDiff])

  // Judge notes for what's on screen: a tapped chip contributes its OWN
  // round's notes for the primary at the revision that round judged; with
  // no chip active, every round's matching notes anchor (the picker era's
  // #1139 behaviour, kept). The anchoring machinery itself - anchorNotes
  // -> notesInRange -> the renderers - is untouched.
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
    load()
    loadJudgeBodies()
    loadRevisions()
    loadContent()
    loadDiff()
  }, [load, loadJudgeBodies, loadRevisions, loadContent, loadDiff])

  // Live SSE follow (#1114): chatStore.subscribe already fans out to any
  // listener while mounted (same seam DagNode/NodePopup use) - no new pub/sub.
  // Refs, not deps, because the listener is registered once per chatId and
  // must read state as it is at event time, not as it was at subscribe time.
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
    Promise.resolve(run()).then(() => {
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
        withScrollPreserved(load)
        if (rev.id === primaryIdRef.current) {
          if (atLatestRef.current) currentRevRef.current = null
          withScrollPreserved(loadRevisions)
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

  const displayText = content != null ? prettyText(content, primary?.class) : null
  const lines = useMemo(() => (displayText != null ? displayText.split('\n') : []), [displayText])
  const { byLine, unanchored } = useMemo(() => anchorNotes(lines, notes), [lines, notes])
  const isStructured = primary?.class === 'structured'
  const parsedJson = useMemo(() => (isStructured && content != null ? tryParseJSON(content) : undefined), [isStructured, content])

  // "More": every secondary artifact (inputs the node read - dispatch-
  // authored - findings, files, …) as labelled groups behind bottom
  // disclosures; each item expands inline into the same renderer stack.
  const moreGroups = useMemo(
    () => groupSecondary(nodeArtifacts.filter(a => a.kind !== 'judge_round' && a.name !== primaryId)),
    [nodeArtifacts, primaryId],
  )

  // Details: the one place a raw id may appear. onTrigger jumps to the
  // round that produced this revision - but only when that round is one of
  // this node's (a known chip); otherwise it renders as plain text.
  const triggerId = curInfo?.lineage?.trigger_annotation
  const onTrigger =
    triggerId != null && judgeBodies[triggerId] != null
      ? () => activateRound(triggerId)
      : undefined

  // Native <dialog> + showModal(): Esc closes (fires 'cancel' then 'close'),
  // focus is trapped in the top layer, and per the HTML spec the browser
  // itself restores focus to whatever had it before showModal() - the
  // "artifacts" button that opened this - once the dialog closes. No manual
  // focus-trap or focus-restore code needed; onClose (the native 'close'
  // event, fired on Esc AND on our own .close() calls below) is the single
  // place that tells the parent to unmount us, so that restore always
  // finishes before React removes the dialog from the DOM.
  const dialogRef = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    dialogRef.current?.showModal()
  }, [])

  const empty = primary == null
  // Details renders its content only while OPEN: a closed native <details>
  // keeps its content in the DOM (hidden, and still readable by text-
  // scanning tools), and the panel's one rule is that the raw id exists
  // NOWHERE until the disclosure is opened (#1178).
  const [detailsOpen, setDetailsOpen] = useState(false)

  return (
    <dialog
      ref={dialogRef}
      aria-label={`Artifacts for node ${nodeId}`}
      onClose={onClose}
      onClick={e => { if (e.target === dialogRef.current) dialogRef.current?.close() }}
      // Full-height bottom sheet below `sm` - h-dvh, not h-screen, so the
      // sheet reaches the real bottom edge under mobile browser chrome
      // (same fix as the app shell, #1177) - overridden back to the centered
      // card at sm+. One component tree at every width: the only
      // width-conditional code left is this container class.
      className="m-0 w-screen h-dvh sm:m-auto sm:w-full sm:max-w-4xl sm:h-[min(32rem,85vh)] max-h-[100vh] sm:max-h-[85vh] p-0 border-0 rounded-none sm:rounded-2xl bg-transparent backdrop:bg-black/40"
    >
      <div
        className="relative flex flex-col w-full h-full overflow-hidden rounded-none sm:rounded-2xl bg-gray-50 dark:bg-gray-900 shadow-xl"
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
              className="flex h-11 w-11 items-center justify-center rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-500 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
            >
              ↻
            </button>
            <button
              onClick={() => dialogRef.current?.close()}
              aria-label="Close"
              className="flex h-11 w-11 items-center justify-center rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-500 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
            >
              ✕
            </button>
          </div>
        </header>

        {/* Judge-round timeline: pinned directly under the header,
            non-scrolling block, horizontal scroll when the rounds overflow
            390px. Chips are buttons, never a picker: tapping one shows that
            round's notes on the revision it judged. */}
        {chips.length > 0 && !empty && (
          <div
            role="group"
            aria-label="Judge rounds"
            className="shrink-0 flex items-center gap-1.5 overflow-x-auto px-4 py-1.5 border-b border-gray-200 dark:border-gray-700"
          >
            {chips.map(({ id, b }) => {
              const active = activeRoundId === id
              return (
                <button
                  key={id}
                  type="button"
                  aria-pressed={active}
                  aria-label={`Round ${b.round}, ${b.passed == null ? 'no verdict' : b.passed ? 'passed' : 'failed'}${b.score != null ? `, score ${b.score}` : ''}`}
                  onClick={() => activateRound(id)}
                  className={`shrink-0 inline-flex items-center gap-1 h-11 sm:h-8 px-3 rounded-full border text-xs transition-colors ${
                    active
                      ? 'border-blue-400 dark:border-blue-500 bg-blue-50 dark:bg-blue-900/30'
                      : 'border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 hover:bg-gray-100 dark:hover:bg-gray-700'
                  }`}
                >
                  <span className="font-medium text-gray-700 dark:text-gray-200">Round {b.round}</span>
                  <span aria-hidden="true" className="text-gray-400 dark:text-gray-500">·</span>
                  <span className={b.passed == null ? 'text-gray-500 dark:text-gray-400' : b.passed ? 'text-green-700 dark:text-green-400' : 'text-red-600 dark:text-red-400'}>
                    {b.passed == null ? 'no verdict' : b.passed ? 'passed' : 'failed'}
                  </span>
                  {b.score != null && (
                    <>
                      <span aria-hidden="true" className="text-gray-400 dark:text-gray-500">·</span>
                      <span className="text-gray-600 dark:text-gray-300 tabular-nums">{b.score}</span>
                    </>
                  )}
                </button>
              )
            })}
          </div>
        )}

        {/* The single scrolling region: revision bar, the rendered output
            (with judge-note highlights), More, Details. */}
        <div ref={scrollRef} className="flex-1 min-h-0 overflow-y-auto overscroll-contain px-4 sm:px-5 py-3 space-y-3">
          {error && <p className="text-xs text-red-500 dark:text-red-400">{error}</p>}

          {empty ? (
            <div className="flex flex-col items-center justify-center gap-1 min-h-[14rem] text-center px-6">
              {nodeError ? (
                <>
                  <p className="text-sm font-medium text-gray-700 dark:text-gray-200">This node failed before writing its result.</p>
                  <p className="text-xs text-red-600 dark:text-red-400 break-words">{nodeError}</p>
                </>
              ) : (
                <p className="text-sm text-gray-400 dark:text-gray-500">This node hasn't produced anything yet.</p>
              )}
            </div>
          ) : (
            <>
              {revisions.length > 0 && (
                <div className="flex items-center gap-1.5 flex-wrap text-xs">
                  <span aria-live="polite" className="text-gray-600 dark:text-gray-300 tabular-nums">
                    Revision {currentRev ?? '–'} of {revisions.length}
                  </span>
                  <button
                    onClick={() => move(-1)}
                    aria-label="Previous revision"
                    disabled={revIdx == null || revIdx <= 0}
                    className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
                  >
                    ← Prev
                  </button>
                  <button
                    onClick={() => move(1)}
                    aria-label="Next revision"
                    disabled={revIdx == null || revIdx >= revisions.length - 1}
                    className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
                  >
                    Next →
                  </button>
                  <button
                    onClick={() => setDiffOn(d => !d)}
                    aria-pressed={diffActive}
                    disabled={diffDisabledReason != null}
                    title={diffDisabledReason ?? undefined}
                    className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
                  >
                    Diff
                  </button>
                  {diffDisabledReason != null && (
                    <span className="text-gray-400 dark:text-gray-500">{diffDisabledReason}</span>
                  )}
                  <button
                    onClick={() => setRawView(r => !r)}
                    aria-pressed={rawView && !diffActive}
                    disabled={diffActive}
                    className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
                  >
                    Raw
                  </button>
                  {displayText != null && <CopyButton text={displayText} label="Copy artifact text" />}
                </div>
              )}

              {diffActive && diffText != null ? (
                <DiffView text={diffText} />
              ) : displayText == null ? (
                <p className="text-xs text-gray-400 dark:text-gray-500">Loading…</p>
              ) : rawView && !diffActive ? (
                <ArtifactLines lines={lines} byLine={byLine} activeNote={activeNote} onSelectNote={setActiveNote} />
              ) : isStructured ? (
                parsedJson !== undefined ? (
                  <JsonView data={parsedJson} kind={primary?.kind} />
                ) : (
                  <ArtifactLines lines={lines} byLine={byLine} activeNote={activeNote} onSelectNote={setActiveNote} />
                )
              ) : (
                <ArtifactMarkdown text={content ?? ''} byLine={byLine} activeNote={activeNote} onSelectNote={setActiveNote} />
              )}

              {activeNote && (
                <div className="rounded-lg border border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-900/20 px-3 py-2 text-xs text-amber-800 dark:text-amber-300">
                  {activeNote.criterion && <span className="font-semibold mr-1">{activeNote.criterion}:</span>}
                  {activeNote.text}
                </div>
              )}

              {unanchored.length > 0 && (
                <div>
                  <span className="text-[10px] font-semibold text-gray-400 dark:text-gray-500 uppercase tracking-wide">
                    Unanchored notes
                  </span>
                  <ul className="mt-1 space-y-1">
                    {unanchored.map((n, i) => (
                      <li key={i} className="text-xs text-gray-500 dark:text-gray-400">
                        {n.criterion && <span className="font-semibold mr-1">{n.criterion}:</span>}
                        {n.text}
                      </li>
                    ))}
                  </ul>
                </div>
              )}

              {moreGroups.length > 0 && (
                <div className="space-y-2">
                  {moreGroups.map(g => (
                    <details key={g.label} className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg">
                      <summary className="cursor-pointer select-none px-3 py-2 text-xs font-medium text-gray-600 dark:text-gray-300">
                        {g.label} ({g.items.length})
                      </summary>
                      <div className="px-3 pb-2 space-y-1">
                        {g.items.map((a, i) => (
                          <MoreItem key={a.name} chatId={chatId} artifact={a} ordinal={i + 1} />
                        ))}
                      </div>
                    </details>
                  ))}
                </div>
              )}

              {curInfo && (
                <details
                  onToggle={e => setDetailsOpen(e.currentTarget.open)}
                  className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg"
                >
                  <summary className="cursor-pointer select-none px-3 py-2 text-[10px] font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">
                    Details
                  </summary>
                  {detailsOpen && (
                    <div className="px-3 pb-3">
                      <MetaRow label="task">
                        <p className="line-clamp-6 whitespace-pre-wrap">{nodeTask}</p>
                      </MetaRow>
                      <ArtifactMetadata
                        summary={primary}
                        revision={curInfo}
                        revisionCount={revisions.length}
                        onTrigger={onTrigger}
                        url={artifactUrl(chatId, primary.name, curInfo.revision)}
                      />
                    </div>
                  )}
                </details>
              )}
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

// tryParseJSON is the tree view's own parse (separate from prettyText's,
// which already swallows a parse error into a raw-text fallback) - returns
// undefined rather than throwing so the tree view can fall back to raw text
// for a structured artifact whose bytes are, unexpectedly, not valid JSON.
function tryParseJSON(raw: string): unknown {
  try {
    return JSON.parse(raw)
  } catch {
    return undefined
  }
}

// MoreItem is one secondary artifact expanded inline inside "More": the
// same renderer stack as the primary output, its own Revision N-of-M
// prev/next, no selects at any level (#1178). Revisions are fetched lazily
// on first expand.
function MoreItem({ chatId, artifact, ordinal }: { chatId: string; artifact: ArtifactSummary; ordinal: number }) {
  const [open, setOpen] = useState(false)
  const [revisions, setRevisions] = useState<ArtifactRevisionInfo[] | null>(null)
  const [idx, setIdx] = useState<number | null>(null)
  const [content, setContent] = useState<string | null>(null)
  const [rawView, setRawView] = useState(false)
  const revisionsToken = useRef(0)
  const contentToken = useRef(0)

  useEffect(() => {
    if (!open || revisions !== null) return
    const token = ++revisionsToken.current
    api.listArtifactRevisions(chatId, artifact.name)
      .then(r => {
        if (token !== revisionsToken.current) return
        const asc = toAscending(r.data ?? [])
        setRevisions(asc)
        setIdx(prev => prev ?? (asc.length > 0 ? asc.length - 1 : null))
      })
      .catch(() => { /* a failed fetch leaves the row to retry on re-expand */ })
  }, [open, chatId, artifact.name, revisions])

  const currentRev = idx != null && revisions != null ? revisions[idx]?.revision ?? null : null
  useEffect(() => {
    if (!open || currentRev == null) { setContent(null); return }
    const token = ++contentToken.current
    api.getArtifactText(chatId, artifact.name, currentRev)
      .then(t => { if (token === contentToken.current) setContent(t) })
      .catch(() => { if (token === contentToken.current) setContent(null) })
  }, [open, chatId, artifact.name, currentRev])

  const displayText = content != null ? prettyText(content, artifact.class) : null
  const lines = useMemo(() => (displayText != null ? displayText.split('\n') : []), [displayText])
  const isStructured = artifact.class === 'structured'
  const parsedJson = useMemo(() => (isStructured && content != null ? tryParseJSON(content) : undefined), [isStructured, content])
  const emptyByLine = useMemo(() => new Map<number, JudgeNote[]>(), [])

  return (
    <div className="border-t border-gray-100 dark:border-gray-700 first:border-t-0 pt-1">
      <button
        type="button"
        onClick={() => setOpen(o => !o)}
        aria-expanded={open}
        className="w-full text-left rounded px-1.5 py-2 text-xs text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700/50"
      >
        {moreItemLabel(artifact, ordinal)}
      </button>
      {open && (
        <div className="mt-1 space-y-2">
          <div className="flex items-center gap-1.5 flex-wrap text-xs">
            <span aria-live="polite" className="text-gray-600 dark:text-gray-300 tabular-nums">
              Revision {currentRev ?? '–'} of {revisions?.length ?? 0}
            </span>
            <button
              onClick={() => setIdx(i => (i != null && i > 0 ? i - 1 : i))}
              aria-label="Previous revision"
              disabled={idx == null || idx <= 0}
              className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
            >
              ← Prev
            </button>
            <button
              onClick={() => setIdx(i => (i != null && revisions != null && i < revisions.length - 1 ? i + 1 : i))}
              aria-label="Next revision"
              disabled={idx == null || revisions == null || idx >= revisions.length - 1}
              className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300 disabled:opacity-40 disabled:cursor-default"
            >
              Next →
            </button>
            <button
              onClick={() => setRawView(r => !r)}
              aria-pressed={rawView}
              className="inline-flex h-11 sm:h-8 px-3 items-center rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-600 dark:text-gray-300"
            >
              Raw
            </button>
            {displayText != null && <CopyButton text={displayText} label="Copy artifact text" />}
          </div>
          {content == null ? (
            <p className="text-xs text-gray-400 dark:text-gray-500">Loading…</p>
          ) : rawView ? (
            <ArtifactLines lines={lines} byLine={emptyByLine} activeNote={null} onSelectNote={() => {}} />
          ) : isStructured ? (
            parsedJson !== undefined ? (
              <JsonView data={parsedJson} kind={artifact.kind} />
            ) : (
              <ArtifactLines lines={lines} byLine={emptyByLine} activeNote={null} onSelectNote={() => {}} />
            )
          ) : (
            <ArtifactMarkdown text={content ?? ''} byLine={emptyByLine} activeNote={null} onSelectNote={() => {}} />
          )}
        </div>
      )}
    </div>
  )
}

// ArtifactLines is the monospace "Raw" fallback view: an exact line list, one
// judge-note highlight per line index. #1114 made ArtifactMarkdown/JsonView
// the DEFAULT (real rendering, real trees) - this stays reachable behind the
// Raw toggle for when the rendered view's own highlighting (data-line-based,
// see ArtifactMarkdown) doesn't anchor a note precisely enough to find it.
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
                  className={`ml-1.5 inline-flex h-4 w-4 items-center justify-center rounded-full text-[10px] leading-none cursor-pointer ${
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
// `.prose h1` cascade (review #1139 cosmetic follow-up: headings rendered at
// body size) - this tree sits inside DagView's `<div className="... not-
// prose">` (escaping an ANCESTOR .prose bubble further up), and Tailwind
// Typography's own selector (`.prose :where(h1):not(:where([class~="not-
// prose"] *))`) excludes EVERY descendant of a not-prose ancestor from ANY
// .prose styling, including a nested one - so relying on the cascade here
// was never going to work as long as this component renders inside DagView.
const HEADING_CLASS: Record<string, string> = {
  h1: 'text-base font-bold mt-3 mb-1.5',
  h2: 'text-sm font-bold mt-3 mb-1.5',
  h3: 'text-sm font-semibold mt-2 mb-1',
  h4: 'text-xs font-semibold mt-2 mb-1',
  h5: 'text-xs font-semibold mt-2 mb-1',
  h6: 'text-xs font-semibold mt-2 mb-1 text-gray-500 dark:text-gray-400',
}

// notesInRange attaches a note to the block whose source range CONTAINS the
// anchor line, not just the block whose FIRST line matches it (review
// #1139: a note anchored mid-paragraph used to render nowhere in the
// default markdown view and wasn't listed as unanchored either, since
// anchorNotes had already placed it in byLine - just under a line index
// this component never checked). blockquote is excluded: CommonMark nests a
// `> quote` as <blockquote><p>...</p></blockquote>, and both wrapper and
// child share the same line range, so blockquote would double-render every
// note its inner paragraph already claims.
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

// ArtifactMarkdown renders a blob artifact through the same react-markdown
// pipeline AssistantText uses (headings, tables, code blocks with highlight +
// copy - #1114), while keeping judge notes anchorable: remark/rehype already
// keep each node's source line (`node.position.start.line`, 1-based, used
// elsewhere in this codebase - see AgentParts' mermaid-detection `pre`
// override) - ponytail: stamping that straight onto the rendered element via
// the existing `components` override achieves the same "data-line on block
// elements" result a dedicated rehype plugin would, with no new plugin.
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
              className={`not-prose ml-1.5 inline-flex h-4 w-4 items-center justify-center rounded-full text-[10px] leading-none cursor-pointer align-middle ${
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
  // numbers (what byLine/data-line anchor on) are unaffected; only within-
  // line offsets shift, which nothing here reads.
  const fixed = useMemo(() => escapeUnmatchedBackticks(text), [text])

  return (
    <div className="prose prose-sm dark:prose-invert max-w-none break-words bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-4 py-3">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlight]}
        components={components}
      >{fixed}</ReactMarkdown>
    </div>
  )
}

// typedValue renders one JSON leaf value, colored by type - the same palette
// DiffView/JudgeCard already use elsewhere in this file/AgentParts, so a
// number/string/boolean reads the same way across the app, not just here.
function typedValue(v: unknown) {
  if (v === null) return <span className="text-gray-400 dark:text-gray-500 italic">null</span>
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
        {k != null && <span className="text-gray-400 dark:text-gray-500">{k}: </span>}
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
        {k != null && <span className="text-gray-400 dark:text-gray-500">{k}: </span>}
        <span className="text-gray-500 dark:text-gray-400">
          {isArray ? `Array(${entries.length})` : `Object{${entries.length}}`}
        </span>
      </summary>
      <div className="ml-3 pl-2 border-l border-gray-200 dark:border-gray-700">
        {entries.length === 0 && <div className="py-0.5 text-gray-400 dark:text-gray-500 italic">empty</div>}
        {entries.map(([ck, cv]) => <JsonNode key={ck} k={ck} v={cv} />)}
      </div>
    </details>
  )
}

// judgeRoundSummary is the small passed/score + per-criterion chip header
// shown above the tree for a judge_round artifact specifically - the one
// structured kind whose top-level shape is worth a glance without expanding
// anything (owner request on #1114). Kept for a judge_round body rendered
// through JsonView (e.g. from a "More" entry on a node whose judge rounds
// survive as artifacts); the panel's own timeline uses the chip row instead.
function judgeRoundSummary(data: unknown) {
  if (data == null || typeof data !== 'object') return null
  const d = data as { passed?: boolean; score?: number; criteria?: { name?: string; score?: number }[] }
  if (d.passed == null && d.score == null && !d.criteria) return null
  return (
    <div className="flex flex-wrap items-center gap-1.5 mb-2 text-xs">
      {d.passed != null && (
        <span className={`font-medium ${d.passed ? 'text-green-700 dark:text-green-400' : 'text-red-600 dark:text-red-400'}`}>
          {d.passed ? '✓ passed' : '✗ failed'}
        </span>
      )}
      {/* d.score (JudgeRoundRecord.Score) is a real 0-1 fraction - shown
          as-is, matching the timeline chips. Per-criterion scores are NOT:
          judge criteria are 0-3 by design (#941 scaleSpec,
          internal/vetting/envelope.go) while a deterministic check like
          cites_sources keeps its own native 0-1 scale, and
          buildJudgeRoundRecord copies criteria[].score through un-normalized
          with no scale field to convert by - a raw number (e.g. "evidence
          2.5") is the only display that isn't a guess. */}
      {d.score != null && <span className="text-gray-500 dark:text-gray-400 tabular-nums">{d.score}</span>}
      {d.criteria?.map((c, i) => (
        <span key={i} title={c.name} className="rounded-full bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300 px-2 py-0.5">
          {c.name}{c.score != null ? ` ${c.score}` : ''}
        </span>
      ))}
    </div>
  )
}

// JsonView is the collapsible key/value tree default view for a structured
// artifact (#1114 owner request) - the pretty-printed code block moved to
// the "Raw" toggle (ArtifactLines).
function JsonView({ data, kind }: { data: unknown; kind?: string }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2 overflow-x-auto">
      {kind === 'judge_round' && judgeRoundSummary(data)}
      <JsonNode v={data} />
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

// fmtAbsoluteShort renders "Jun 21, 14:32" - shown INLINE next to the
// relative time (review #1139: a hover-only `title` tooltip is unreachable
// on a touch device, and this panel's own mobile pass makes touch the
// primary surface, not an edge case). The full ISO string still lives in
// `title` for a pointer user who wants to copy it exactly.
function fmtAbsoluteShort(iso: string): string {
  return new Date(iso).toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

// MetaRow is one key/value line, same visual language as JsonNode's leaves -
// the metadata block is "the JSON tree's styling" applied to a fixed set of
// fields (owner request on #1114) rather than a second, differently-styled table.
function MetaRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="py-0.5 text-xs flex gap-1">
      <span className="text-gray-400 dark:text-gray-500 shrink-0">{label}:</span>
      <span className="text-gray-800 dark:text-gray-100 break-words min-w-0">{children}</span>
    </div>
  )
}

// ArtifactMetadata is the panel's Details disclosure content (#1178): the
// one place the raw id, kind, class, per-revision lineage, timestamps and
// the REST link appear. Plain block - the enclosing <details> in the panel
// owns the disclosure. The trigger row (the judge_round that produced this
// revision) is a jump to that round's chip when the round is one of this
// node's; plain text otherwise.
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
      {l?.node_id && <MetaRow label="node">{l.node_id}</MetaRow>}
      {l?.round != null && <MetaRow label="round">{l.round}</MetaRow>}
      {l?.parent_revision != null && <MetaRow label="parent revision">{l.parent_revision}</MetaRow>}
      {l?.trigger_annotation && (
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
      {l?.head_sha && (
        <MetaRow label="head sha">
          <span className="inline-flex items-center gap-1">
            <span className="font-mono">{l.head_sha.slice(0, 12)}</span>
            <CopyButton text={l.head_sha} label="Copy head sha" />
          </span>
        </MetaRow>
      )}
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
