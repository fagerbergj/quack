import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api'
import type { ArtifactSummary, ArtifactRevisionInfo } from '../api'
import { CopyButton } from './CopyButton'

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
      idx = lines.findIndex(l => l.includes(note.ref.snippet as string))
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

  const nodeArtifacts = useMemo(
    () => summaries.filter(s => s.lineage?.node_id === nodeId),
    [summaries, nodeId],
  )
  // Inputs are dispatch-authored (design V4 §4.4); everything else this node
  // wrote (worker/judge/gate/delivery) is an output.
  const inputs = useMemo(() => nodeArtifacts.filter(s => s.lineage?.author === 'dispatch'), [nodeArtifacts])
  const outputs = useMemo(() => nodeArtifacts.filter(s => s.lineage?.author !== 'dispatch'), [nodeArtifacts])
  const judgeRoundIds = useMemo(() => nodeArtifacts.filter(s => s.kind === 'judge_round').map(s => s.name), [nodeArtifacts])

  useEffect(() => {
    if (!selectedId) { setRevisions([]); setSelectedRev(null); return }
    api.listArtifactRevisions(chatId, selectedId)
      .then(r => {
        setRevisions(r.data ?? [])
        setSelectedRev(r.data?.[0]?.revision ?? null)
      })
      .catch(e => setError(String(e)))
  }, [chatId, selectedId])

  useEffect(() => {
    setActiveNote(null)
    if (!selectedId || selectedRev == null) { setContent(null); return }
    api.getArtifactText(chatId, selectedId, selectedRev).then(setContent).catch(e => setError(String(e)))
  }, [chatId, selectedId, selectedRev])

  useEffect(() => {
    if (!diffOn || !selectedId || selectedRev == null || diffAgainst == null || diffAgainst === selectedRev) {
      setDiffText(null)
      return
    }
    const from = Math.min(selectedRev, diffAgainst)
    const to = Math.max(selectedRev, diffAgainst)
    api.diffArtifactRevisions(chatId, selectedId, from, to).then(setDiffText).catch(e => setError(String(e)))
  }, [diffOn, chatId, selectedId, selectedRev, diffAgainst])

  // Judge notes referencing exactly the artifact+revision on screen - pulled
  // from every judge_round artifact's latest content, not just one, since a
  // node can run several judge rounds each writing its own judge_round id.
  useEffect(() => {
    if (!selectedId || selectedRev == null || judgeRoundIds.length === 0) { setJudgeNotes([]); return }
    let cancelled = false
    Promise.all(
      judgeRoundIds.map(id =>
        api.getArtifactText(chatId, id)
          .then(t => JSON.parse(t) as JudgeRoundContent)
          .catch(() => null),
      ),
    ).then(rounds => {
      if (cancelled) return
      const notes = rounds
        .filter((r): r is JudgeRoundContent => r != null)
        .flatMap(r => r.notes ?? [])
        .filter(n => n.ref.artifact_id === selectedId && n.ref.revision === selectedRev)
      setJudgeNotes(notes)
    })
    return () => { cancelled = true }
  }, [chatId, selectedId, selectedRev, judgeRoundIds])

  const selectedSummary = nodeArtifacts.find(s => s.name === selectedId)
  const displayText = content != null ? prettyText(content, selectedSummary?.class) : null
  const lines = useMemo(() => (displayText != null ? displayText.split('\n') : []), [displayText])
  const { byLine, unanchored } = useMemo(() => anchorNotes(lines, judgeNotes), [lines, judgeNotes])

  function selectArtifact(id: string) {
    setSelectedId(id)
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

  return (
    <dialog
      ref={dialogRef}
      aria-label={`Artifacts for node ${nodeId}`}
      onClose={onClose}
      onClick={e => { if (e.target === dialogRef.current) dialogRef.current?.close() }}
      className="m-auto w-full max-w-4xl max-h-[85vh] p-0 border-0 rounded-2xl bg-transparent backdrop:bg-black/40"
    >
      <div
        className="relative flex w-full max-h-[85vh] overflow-hidden rounded-2xl bg-gray-50 dark:bg-gray-900 shadow-xl"
      >
        {/* Left: input/output artifact list */}
        <div className="w-56 shrink-0 overflow-y-auto border-r border-gray-200 dark:border-gray-700 py-3 px-2 space-y-3">
          <ArtifactGroup title="Inputs" items={inputs} selectedId={selectedId} onSelect={selectArtifact} />
          <ArtifactGroup title="Outputs" items={outputs} selectedId={selectedId} onSelect={selectArtifact} />
          {nodeArtifacts.length === 0 && (
            <p className="text-xs text-gray-400 dark:text-gray-500 px-1">No artifacts for this node yet.</p>
          )}
        </div>

        {/* Right: revision picker, diff toggle, content, highlights */}
        <div className="flex-1 min-w-0 overflow-y-auto px-5 py-3 space-y-3">
          <div className="flex items-center justify-between">
            <h2 className="text-xs font-semibold text-gray-700 dark:text-gray-200 truncate">
              {selectedId ?? 'Select an artifact'}
            </h2>
            <div className="flex items-center gap-1 shrink-0">
              <button
                onClick={load}
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
              {displayText != null && <CopyButton text={displayText} label="Copy artifact text" />}
            </div>
          )}

          {diffOn && diffAgainst != null && diffAgainst === selectedRev ? (
            <p className="text-xs text-gray-400 dark:text-gray-500">Pick a different revision to diff against - it's the same one shown above.</p>
          ) : diffOn && diffText != null ? (
            <DiffView text={diffText} />
          ) : displayText != null ? (
            <ArtifactLines lines={lines} byLine={byLine} activeNote={activeNote} onSelectNote={setActiveNote} />
          ) : selectedId ? (
            <p className="text-xs text-gray-400 dark:text-gray-500">Loading…</p>
          ) : null}

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
        {items.map(a => (
          <li key={a.name}>
            <button
              onClick={() => onSelect(a.name)}
              aria-current={selectedId === a.name}
              className={`w-full text-left truncate rounded px-1.5 py-1 text-xs ${
                selectedId === a.name
                  ? 'bg-gray-200 dark:bg-gray-700 text-gray-800 dark:text-gray-100'
                  : 'text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-800'
              }`}
              title={a.name}
            >
              {a.kind ?? a.name}
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}

// ArtifactLines renders the revision as a monospace line list rather than
// through the markdown pipeline (design V4 says "rendered text" but doesn't
// prescribe HTML markdown output) - judge notes anchor to a LINE, and
// react-markdown's AST gives no stable line->DOM mapping to hang a
// highlight/click off without a custom rehype plugin. ponytail: full
// markdown rendering (headings, tables) is dropped for anchoring precision;
// add a rehype line-mapping plugin if rich rendering + highlights both turn
// out to be needed at once.
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
          const notes = byLine.get(i)
          const highlighted = !!notes && notes.length > 0
          const isActive = highlighted && notes!.includes(activeNote as JudgeNote)
          return (
            <div
              key={i}
              role={highlighted ? 'button' : undefined}
              tabIndex={highlighted ? 0 : undefined}
              aria-label={highlighted ? `Judge note on line ${i + 1}: ${notes![0].text}` : undefined}
              onClick={highlighted ? () => onSelectNote(notes![0]) : undefined}
              onKeyDown={highlighted ? (e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelectNote(notes![0]) } }) : undefined}
              className={`px-3 py-0.5 whitespace-pre-wrap break-words ${
                highlighted
                  ? `cursor-pointer ${isActive ? 'bg-amber-200 dark:bg-amber-800/60' : 'bg-amber-100 dark:bg-amber-900/30 hover:bg-amber-200 dark:hover:bg-amber-800/50'}`
                  : ''
              }`}
            >
              {line || ' '}
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
