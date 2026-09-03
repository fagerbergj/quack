import { createElement, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeRaw from 'rehype-raw'
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize'
import rehypeHighlight from 'rehype-highlight'
import 'highlight.js/styles/github-dark.css'
import type { Element } from 'hast'
import { api } from '../api'
import type { ArtifactSummary, ArtifactRevisionInfo } from '../api'
import { CopyButton } from './CopyButton'
import { CopyablePre } from './CopyablePre'

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

// NARROW_QUERY matches Tailwind's `sm` breakpoint (640px) so the JS-driven
// layout switch (list -> select, metadata collapsed by default) lines up
// with the pure-CSS responsive classes elsewhere in this file (dialog
// sizing, tap-target padding) - #1114 mobile pass.
const NARROW_QUERY = '(max-width: 639px)'

// useIsNarrow drives the STRUCTURAL swap (a <select> instead of the sidebar
// list) - unlike pure Tailwind responsive classes, this needs to be a real
// JS check so it's actually testable (jsdom never evaluates @media queries)
// and so the two layouts render different elements, not just different
// classes on the same one.
function useIsNarrow(): boolean {
  const [narrow, setNarrow] = useState(() => (typeof window !== 'undefined' ? window.matchMedia(NARROW_QUERY).matches : false))
  useEffect(() => {
    const mq = window.matchMedia(NARROW_QUERY)
    const onChange = () => setNarrow(mq.matches)
    onChange()
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])
  return narrow
}

interface Props {
  chatId: string
  nodeId: string
  onClose: () => void
}

export function ArtifactPanel({ chatId, nodeId, onClose }: Props) {
  const [summaries, setSummaries] = useState<ArtifactSummary[]>([])
  const [error, setError] = useState<string | null>(null)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [revisions, setRevisions] = useState<ArtifactRevisionInfo[]>([])
  const [selectedRev, setSelectedRev] = useState<number | null>(null)
  const [diffOn, setDiffOn] = useState(false)
  const [diffAgainst, setDiffAgainst] = useState<number | null>(null)
  // rawView: the monospace line-list is now the FALLBACK view (also what a
  // judge-note highlight needs a stable line index for - #1114 keeps it as a
  // toggle rather than dropping it), not the default; structured kinds
  // default to the collapsible tree, blobs to rendered markdown.
  const [rawView, setRawView] = useState(false)
  const [content, setContent] = useState<string | null>(null)
  const [diffText, setDiffText] = useState<string | null>(null)
  const [judgeNotes, setJudgeNotes] = useState<JudgeNote[]>([])
  const [activeNote, setActiveNote] = useState<JudgeNote | null>(null)

  const load = useCallback(() => {
    api.listChatArtifacts(chatId).then(l => setSummaries(l.data ?? [])).catch(e => setError(String(e)))
  }, [chatId])
  useEffect(() => { load() }, [load])
  // ponytail: no live SSE wiring here - artifact_revision/artifact_judge_round
  // are emitted by #1092 (a separate, in-progress branch), and agentStream's
  // dispatch is a typed per-event switch, not a generic pub/sub a late
  // subscriber can tap. Reload via REST only for now (the explicit Refresh
  // button below); wire a real handler once #1092's payload shape exists to
  // test against.

  // Membership is by the LATEST revision's lineage.node_id (ArtifactSummary
  // only carries that revision's lineage - see toArtifactSummary in
  // internal/server/rest/artifacts.go), not "any revision this node wrote".
  // If a later node revises an artifact (e.g. a judge writes revision 2 of a
  // worker's `finding`), it moves to the reviser's panel and disappears from
  // the original author's - a real gap against a "everything this node
  // wrote is an output" reading of design V4, open as a question on #1094's
  // review pending a spec answer. Fixing it needs per-revision lineage
  // (GET .../revisions) fetched for every artifact in the chat up front,
  // which doesn't scale to "one click opens a panel" - documented here
  // rather than silently wrong.
  const nodeArtifacts = useMemo(
    () => summaries.filter(s => s.lineage?.node_id === nodeId),
    [summaries, nodeId],
  )
  // Inputs are dispatch-authored (design V4 §4.4); judge_round gets its own
  // group (it's provenance ABOUT the outputs, not one) - owner feedback
  // (#1114): "Judge rounds" alongside Inputs/Outputs. Everything else this
  // node wrote (worker/gate/delivery) is a plain output.
  const inputs = useMemo(() => nodeArtifacts.filter(s => s.lineage?.author === 'dispatch'), [nodeArtifacts])
  const judgeRounds = useMemo(() => nodeArtifacts.filter(s => s.kind === 'judge_round'), [nodeArtifacts])
  const outputs = useMemo(
    () => nodeArtifacts.filter(s => s.lineage?.author !== 'dispatch' && s.kind !== 'judge_round'),
    [nodeArtifacts],
  )
  const judgeRoundIds = useMemo(() => judgeRounds.map(s => s.name), [judgeRounds])

  // Auto-select the sole artifact (#1114 owner feedback): with only one
  // thing to look at, the empty state is just an extra click, not a choice.
  useEffect(() => {
    if (selectedId == null && nodeArtifacts.length === 1) {
      selectArtifact(nodeArtifacts[0].name)
    }
  }, [nodeArtifacts, selectedId])

  // loadRevisions/loadContent/loadDiff/loadJudgeNotes are each an effect body
  // pulled out to a stable callback, so refresh() (below) can re-run every
  // fetch the panel currently has in flight instead of just the artifact
  // list - the refresh button used to only re-run `load()`, leaving a
  // revision written while the panel was open invisible until the user
  // re-selected the artifact.
  // Each loader below carries its own token ref (the judgeNotesToken
  // pattern, extended here): a fast artifact switch can let a slow PRIOR
  // response arrive after a newer request already started, applying stale
  // data over what's now on screen. Checked in both the success and error
  // path - a late error must not clobber a since-succeeded newer fetch either.
  const revisionsToken = useRef(0)
  const loadRevisions = useCallback(() => {
    if (!selectedId) { setRevisions([]); setSelectedRev(null); return }
    const token = ++revisionsToken.current
    return api.listArtifactRevisions(chatId, selectedId)
      .then(r => {
        if (token !== revisionsToken.current) return
        setError(null)
        setRevisions(r.data ?? [])
        // Keep the currently viewed revision selected across a refresh if it
        // still exists; only a fresh selectArtifact() (which nulls
        // selectedRev) or a revision that's since vanished falls back to latest.
        setSelectedRev(prev => (prev != null && r.data?.some(rv => rv.revision === prev) ? prev : (r.data?.[0]?.revision ?? null)))
      })
      .catch(e => { if (token === revisionsToken.current) setError(String(e)) })
  }, [chatId, selectedId])
  useEffect(() => { loadRevisions() }, [loadRevisions])

  const contentToken = useRef(0)
  const loadContent = useCallback(() => {
    setActiveNote(null)
    // selectedRev is reset to null by selectArtifact on every artifact
    // switch, and only ever set back by loadRevisions settling on the NEW
    // artifact's own latest (or still-valid) revision - so this never fires
    // with a stale revision number left over from the previously selected
    // artifact (the cause of a since-fixed stale 404 banner: switching
    // artifacts used to fire this effect in the same commit with the old
    // artifact's selectedRev, before the revisions effect had a chance to
    // update it).
    if (!selectedId || selectedRev == null) { setContent(null); return }
    const token = ++contentToken.current
    return api.getArtifactText(chatId, selectedId, selectedRev)
      .then(text => { if (token !== contentToken.current) return; setError(null); setContent(text) })
      .catch(e => { if (token === contentToken.current) setError(String(e)) })
  }, [chatId, selectedId, selectedRev])
  useEffect(() => { loadContent() }, [loadContent])

  const diffToken = useRef(0)
  const loadDiff = useCallback(() => {
    if (!diffOn || !selectedId || selectedRev == null || diffAgainst == null || diffAgainst === selectedRev) {
      setDiffText(null)
      // Also clear error: otherwise a 413/415 from a PRIOR diff attempt
      // stays rendered after unchecking Diff (or Refresh, which re-runs this
      // same branch) - only re-selecting an artifact or the revision used to
      // clear it.
      setError(null)
      return
    }
    const from = Math.min(selectedRev, diffAgainst)
    const to = Math.max(selectedRev, diffAgainst)
    const token = ++diffToken.current
    return api.diffArtifactRevisions(chatId, selectedId, from, to)
      .then(text => { if (token !== diffToken.current) return; setError(null); setDiffText(text) })
      .catch(e => { if (token === diffToken.current) setError(String(e)) })
  }, [diffOn, chatId, selectedId, selectedRev, diffAgainst])
  useEffect(() => { loadDiff() }, [loadDiff])

  // Judge notes referencing exactly the artifact+revision on screen - pulled
  // from every judge_round artifact's latest content, not just one, since a
  // node can run several judge rounds each writing its own judge_round id.
  // judgeNotesToken guards against a race between two overlapping calls (the
  // effect below firing again, or refresh() firing manually mid-flight) -
  // only the most recently STARTED call's result is applied.
  const judgeNotesToken = useRef(0)
  const loadJudgeNotes = useCallback(() => {
    if (!selectedId || selectedRev == null || judgeRoundIds.length === 0) { setJudgeNotes([]); return }
    const token = ++judgeNotesToken.current
    return Promise.all(
      judgeRoundIds.map(id =>
        api.getArtifactText(chatId, id)
          .then(t => JSON.parse(t) as JudgeRoundContent)
          .catch(() => null),
      ),
    ).then(rounds => {
      if (token !== judgeNotesToken.current) return
      const notes = rounds
        .filter((r): r is JudgeRoundContent => r != null)
        .flatMap(r => r.notes ?? [])
        .filter(n => n.ref.artifact_id === selectedId && n.ref.revision === selectedRev)
      setJudgeNotes(notes)
    })
  }, [chatId, selectedId, selectedRev, judgeRoundIds])
  useEffect(() => { loadJudgeNotes() }, [loadJudgeNotes])

  // refresh re-runs every fetch the panel currently has live: the artifact
  // list plus, when something is selected, its revisions/content/diff/notes -
  // not just the list load() alone did before.
  const refresh = useCallback(() => {
    load()
    loadRevisions()
    loadContent()
    loadDiff()
    loadJudgeNotes()
  }, [load, loadRevisions, loadContent, loadDiff, loadJudgeNotes])

  const selectedSummary = nodeArtifacts.find(s => s.name === selectedId)
  const isStructured = selectedSummary?.class === 'structured'
  const displayText = content != null ? prettyText(content, selectedSummary?.class) : null
  const lines = useMemo(() => (displayText != null ? displayText.split('\n') : []), [displayText])
  const { byLine, unanchored } = useMemo(() => anchorNotes(lines, judgeNotes), [lines, judgeNotes])
  const parsedJson = useMemo(() => (isStructured && content != null ? tryParseJSON(content) : undefined), [isStructured, content])
  const selectedRevisionInfo = revisions.find(r => r.revision === selectedRev)

  function selectArtifact(id: string) {
    setSelectedId(id)
    // Root fix for the stale-404-banner bug: without this, the content
    // effect below fires in the same commit with the PREVIOUS artifact's
    // selectedRev (React runs effects in declaration order after one commit,
    // not one per state setter), fetching `newId?revision=oldRev` - a 404 on
    // any artifact whose revision counts differ, whose error then had
    // nothing to ever clear it. Nulling it here makes that effect's own
    // `selectedRev == null` guard skip the bogus fetch until the revisions
    // effect settles on the new artifact's real latest revision.
    setSelectedRev(null)
    setContent(null)
    setError(null)
    setDiffOn(false)
    setDiffAgainst(null)
  }

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

  const isNarrow = useIsNarrow()

  return (
    <dialog
      ref={dialogRef}
      aria-label={`Artifacts for node ${nodeId}`}
      onClose={onClose}
      onClick={e => { if (e.target === dialogRef.current) dialogRef.current?.close() }}
      // Full-screen sheet below `sm` (#1114 mobile pass: 360x740/390x844
      // must show real content, not a postage-stamp dialog) - m-0/rounded-none
      // at the base, overridden back to the centered card at sm+.
      className="m-0 w-screen h-screen sm:m-auto sm:w-full sm:max-w-4xl sm:h-[min(32rem,85vh)] max-h-[100vh] sm:max-h-[85vh] p-0 border-0 rounded-none sm:rounded-2xl bg-transparent backdrop:bg-black/40"
    >
      <div
        className="relative flex flex-col sm:flex-row w-full h-full overflow-hidden rounded-none sm:rounded-2xl bg-gray-50 dark:bg-gray-900 shadow-xl"
      >
        {/* Artifact picker: a top <select> below `sm` (a real structural
            swap, not just responsive classes - #1114 "how am I supposed to
            know to click on 'text'" plus "doesn't fit on mobile" both land
            here), the Inputs/Outputs/Judge rounds sidebar at sm+. */}
        {isNarrow ? (
          <div className="shrink-0 border-b border-gray-200 dark:border-gray-700 p-2">
            <label className="sr-only" htmlFor="artifact-mobile-select">Select an artifact</label>
            <select
              id="artifact-mobile-select"
              value={selectedId ?? ''}
              onChange={e => selectArtifact(e.target.value)}
              className="w-full min-h-11 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 px-2 py-2 text-sm"
            >
              <option value="" disabled>Select an artifact…</option>
              {inputs.length > 0 && (
                <optgroup label="Inputs">
                  {inputs.map(a => <option key={a.name} value={a.name}>{shortId(a.name)}{a.kind ? ` (${a.kind})` : ''}</option>)}
                </optgroup>
              )}
              {outputs.length > 0 && (
                <optgroup label="Outputs">
                  {outputs.map(a => <option key={a.name} value={a.name}>{shortId(a.name)}{a.kind ? ` (${a.kind})` : ''}</option>)}
                </optgroup>
              )}
              {judgeRounds.length > 0 && (
                <optgroup label="Judge rounds">
                  {judgeRounds.map(a => <option key={a.name} value={a.name}>{shortId(a.name)}</option>)}
                </optgroup>
              )}
            </select>
            {nodeArtifacts.length === 0 && (
              <p className="text-xs text-gray-400 dark:text-gray-500 mt-1">No artifacts for this node yet.</p>
            )}
          </div>
        ) : (
          <div className="w-56 shrink-0 overflow-y-auto border-r border-gray-200 dark:border-gray-700 py-3 px-2 space-y-3">
            <ArtifactGroup title="Inputs" items={inputs} selectedId={selectedId} onSelect={selectArtifact} />
            <ArtifactGroup title="Outputs" items={outputs} selectedId={selectedId} onSelect={selectArtifact} />
            <ArtifactGroup title="Judge rounds" items={judgeRounds} selectedId={selectedId} onSelect={selectArtifact} />
            {nodeArtifacts.length === 0 && (
              <p className="text-xs text-gray-400 dark:text-gray-500 px-1">No artifacts for this node yet.</p>
            )}
          </div>
        )}

        {/* Right: revision picker, diff toggle, content, highlights */}
        <div className="flex-1 min-w-0 flex flex-col overflow-y-auto px-5 py-3 space-y-3">
          <div className="flex items-center justify-between">
            <h2 className="text-xs font-semibold text-gray-700 dark:text-gray-200 truncate">
              {selectedId ?? 'Select an artifact'}
            </h2>
            <div className="flex items-center gap-1 shrink-0">
              <button
                onClick={refresh}
                aria-label="Refresh artifacts"
                title="Refresh artifacts"
                className="flex h-7 w-7 items-center justify-center rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-500 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
              >
                ↻
              </button>
              <button
                onClick={() => dialogRef.current?.close()}
                aria-label="Close"
                className="flex h-7 w-7 items-center justify-center rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-500 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
              >
                ✕
              </button>
            </div>
          </div>

          {error && <p className="text-xs text-red-500 dark:text-red-400">{error}</p>}

          {selectedId && revisions.length > 0 && (
            <div className="flex items-center gap-2 flex-wrap text-xs">
              <label htmlFor="artifact-revision-select" className="text-gray-500 dark:text-gray-400">Revision</label>
              <select
                id="artifact-revision-select"
                value={selectedRev ?? ''}
                onChange={e => setSelectedRev(Number(e.target.value))}
                className="rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 px-1.5 py-1"
              >
                {revisions.map(r => (
                  <option key={r.revision} value={r.revision}>
                    r{r.revision}{r.lineage?.round != null ? ` · round ${r.lineage.round}` : ''}
                    {r.lineage?.author ? ` · ${r.lineage.author}` : ''}
                  </option>
                ))}
              </select>

              <label className="flex items-center gap-1 text-gray-500 dark:text-gray-400 ml-2">
                <input type="checkbox" checked={diffOn} onChange={e => setDiffOn(e.target.checked)} disabled={revisions.length < 2} />
                Diff
              </label>
              {diffOn && (
                <select
                  aria-label="Diff against revision"
                  value={diffAgainst ?? ''}
                  onChange={e => setDiffAgainst(Number(e.target.value))}
                  className="rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 px-1.5 py-1"
                >
                  <option value="" disabled>against…</option>
                  {revisions.filter(r => r.revision !== selectedRev).map(r => (
                    <option key={r.revision} value={r.revision}>r{r.revision}</option>
                  ))}
                </select>
              )}
              {!diffOn && displayText != null && (
                <label className="flex items-center gap-1 text-gray-500 dark:text-gray-400 ml-2">
                  <input type="checkbox" checked={rawView} onChange={e => setRawView(e.target.checked)} />
                  Raw
                </label>
              )}
              {displayText != null && <CopyButton text={displayText} label="Copy artifact text" />}
            </div>
          )}

          {selectedSummary && selectedRevisionInfo && (
            <ArtifactMetadata
              summary={selectedSummary}
              revision={selectedRevisionInfo}
              revisionCount={revisions.length}
              onSelectTriggerAnnotation={selectArtifact}
              defaultOpen={!isNarrow}
            />
          )}

          {diffOn && diffAgainst != null && diffAgainst === selectedRev ? (
            <p className="text-xs text-gray-400 dark:text-gray-500">Pick a different revision to diff against - it's the same one shown above.</p>
          ) : diffOn && diffText != null ? (
            <DiffView text={diffText} />
          ) : displayText == null ? (
            selectedId ? (
              <p className="text-xs text-gray-400 dark:text-gray-500">Loading…</p>
            ) : (
              <div className="flex flex-1 h-full min-h-[16rem] items-center justify-center text-center text-xs text-gray-400 dark:text-gray-500 px-6">
                {nodeArtifacts.length === 0
                  ? 'This node has no artifacts yet.'
                  : 'Pick an artifact to view its revisions.'}
              </div>
            )
          ) : rawView ? (
            <ArtifactLines lines={lines} byLine={byLine} activeNote={activeNote} onSelectNote={setActiveNote} />
          ) : isStructured ? (
            parsedJson !== undefined ? (
              <JsonView data={parsedJson} kind={selectedSummary?.kind} />
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
        </div>
      </div>
    </dialog>
  )
}

// shortId keeps a very long id (typically a long content-hash instance, e.g.
// a `finding:<hash>`) readable in the 224px-wide list column - the full id
// is always still on the button's `title` and its accessible name.
function shortId(id: string): string {
  return id.length > 28 ? `${id.slice(0, 14)}…${id.slice(-10)}` : id
}

// ArtifactGroup renders each artifact as a real row, not just its kind
// (#1114 owner feedback: "how am I supposed to know to click on 'text'") -
// the id is the primary label, a small kind chip sits beside it, and a
// second line gives the revision count plus the latest round/author so the
// list is scannable without opening anything. No separate aria-label: the
// button's accessible name is exactly its visible text, id first.
function ArtifactGroup({ title, items, selectedId, onSelect }: {
  title: string
  items: ArtifactSummary[]
  selectedId: string | null
  onSelect: (id: string) => void
}) {
  if (items.length === 0) return null
  return (
    <div>
      <span className="text-[10px] font-semibold text-gray-400 dark:text-gray-500 uppercase tracking-wide px-1">{title}</span>
      <ul className="mt-1 space-y-0.5">
        {items.map(a => {
          const revCount = a.revisions?.length ?? 0
          return (
            <li key={a.name}>
              <button
                onClick={() => onSelect(a.name)}
                aria-current={selectedId === a.name}
                className={`w-full text-left rounded px-1.5 py-1.5 text-xs ${
                  selectedId === a.name
                    ? 'bg-gray-200 dark:bg-gray-700 text-gray-800 dark:text-gray-100'
                    : 'text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-800'
                }`}
                title={a.name}
              >
                <div className="flex items-center gap-1.5 min-w-0">
                  <span className="truncate font-medium">{shortId(a.name)}</span>
                  {a.kind && (
                    <span className="shrink-0 rounded bg-gray-100 dark:bg-gray-800 text-gray-500 dark:text-gray-400 px-1 py-0.5 text-[10px] leading-none">
                      {a.kind}
                    </span>
                  )}
                </div>
                <div className="text-[10px] text-gray-400 dark:text-gray-500 truncate">
                  {revCount} revision{revCount === 1 ? '' : 's'}
                  {a.lineage?.round != null ? ` · round ${a.lineage.round}` : ''}
                  {a.lineage?.author ? ` · ${a.lineage.author}` : ''}
                </div>
              </button>
            </li>
          )
        })}
      </ul>
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
              ? 'text-red-700 dark:text-red-400 bg-red-50 dark:bg-red-900/20'
              : line.startsWith('@@')
                ? 'text-blue-600 dark:text-blue-400'
                : ''
          return (
            <div key={i} className={`px-3 py-0.5 whitespace-pre-wrap break-words ${color}`}>
              {line || ' '}
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
        const notes = line != null ? byLine.get(line - 1) : undefined
        const highlighted = !!notes && notes.length > 0
        return createElement(
          tag,
          {
            ...rest,
            'data-line': line,
            className: highlighted ? 'bg-amber-100 dark:bg-amber-900/30 rounded px-1 -mx-1' : undefined,
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

  return (
    <div className="prose prose-sm dark:prose-invert max-w-none break-words bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-4 py-3">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlight]}
        components={components}
      >{text}</ReactMarkdown>
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
// anything (owner request on #1114).
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
      {d.score != null && <span className="text-gray-500 dark:text-gray-400">{(d.score * 100).toFixed(0)}%</span>}
      {d.criteria?.map((c, i) => (
        <span key={i} title={c.name} className="rounded-full bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300 px-2 py-0.5">
          {c.name}{c.score != null ? ` ${(c.score * 100).toFixed(0)}%` : ''}
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

// fmtRelative renders "3m ago"/"in 2h" etc via the native Intl.RelativeTimeFormat -
// no date library for one small formatter (ponytail).
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

// ArtifactMetadata answers "where is the metadata for each artifact shown" -
// a collapsible block (open by default on desktop, collapsed on a narrow
// viewport via `defaultOpen` - #1114 mobile pass) for the selected
// revision's own row plus its lineage. trigger_annotation is a real link back into this
// same panel (the judge_round artifact that caused this revision), not just
// an id string.
function ArtifactMetadata({ summary, revision, revisionCount, onSelectTriggerAnnotation, defaultOpen }: {
  summary: ArtifactSummary
  revision: ArtifactRevisionInfo
  revisionCount: number
  onSelectTriggerAnnotation: (id: string) => void
  defaultOpen: boolean
}) {
  const l = revision.lineage
  return (
    <details open={defaultOpen} className="text-xs bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-lg px-3 py-2">
      <summary className="cursor-pointer select-none font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide text-[10px]">
        Metadata
      </summary>
      <div className="mt-1">
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
            <button
              type="button"
              onClick={() => onSelectTriggerAnnotation(l.trigger_annotation as string)}
              className="text-blue-600 dark:text-blue-400 hover:underline"
            >
              {l.trigger_annotation}
            </button>
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
            <span title={revision.created_at}>{fmtRelative(revision.created_at)}</span>
          </MetaRow>
        )}
      </div>
    </details>
  )
}
