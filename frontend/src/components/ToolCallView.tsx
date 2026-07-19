import type { ReactNode } from 'react'
import type { ToolCall } from './messageParts'
import { Expandable } from './Expandable'
import {
  prettyJSON,
  str,
  num,
  bool,
  lineDiff,
  parseUnifiedDiff,
  type DiffLine,
} from './toolFormat'

// ToolCallView renders a tool call's body with a per-tool rich view, keyed by
// tool name, falling back to a tidy formatted view (never a raw JSON blob) for
// tools without one. The flagship is edit_file → a before→after diff. Long bodies
// are wrapped in Expandable so a big file / diff / output can't wall off the node.
// Every native quack tool (internal/tools/*) and every name internal/acp/
// translate.go remaps an ACP call onto (edit_file/write_file/read_file/
// run_command/web_fetch) has a case here; anything else — an ACP tool kind we
// don't specially map, or a tool added after this file was last updated —
// falls to GenericView, which is still formatted (key→value / pretty JSON),
// never a raw blob.
export function ToolCallView({ tool }: { tool: ToolCall }) {
  switch (tool.name) {
    case 'edit_file': return <EditFileView tool={tool} />
    case 'write_file': return <WriteFileView tool={tool} />
    case 'read_file': return <ReadFileView tool={tool} />
    case 'delete_path': return <DeletePathView tool={tool} />
    case 'run_command': return <RunCommandView tool={tool} />
    case 'list_dir': return <ListDirView tool={tool} />
    case 'glob': return <GlobView tool={tool} />
    case 'grep': return <GrepView tool={tool} />
    case 'web_search': return <WebSearchView tool={tool} />
    case 'web_fetch': return <WebFetchView tool={tool} />
    case 'ask_advisor': return <AskAdvisorView tool={tool} />
    case 'stage_memory': return <StageMemoryView tool={tool} />
    case 'load_memory': return <LoadMemoryView tool={tool} />
    case 'get_user_choice': return <GetUserChoiceView tool={tool} />
    case 'ask_user': return <AskUserView tool={tool} />
    case 'git_commit': return <GitCommitView tool={tool} />
    case 'git_diff': return <GitDiffView tool={tool} />
    case 'git_log': return <GitLogView tool={tool} />
    case 'git_status': return <GitStatusView tool={tool} />
    case 'git_branch': return <GitBranchView tool={tool} />
    case 'git_push': return <GitPushView tool={tool} />
    default: return <GenericView tool={tool} />
  }
}

// ── shared primitives ─────────────────────────────────────────────────────────

// PathHeader is the mono file-path (+ optional trailing note) heading a file view.
function PathHeader({ path, note }: { path: string; note?: ReactNode }) {
  return (
    <div className="flex items-center gap-2 mb-1">
      <code className="font-mono text-[11px] text-gray-700 dark:text-gray-200 break-all">{path}</code>
      {note != null && <span className="text-[10px] text-gray-400 dark:text-gray-500">{note}</span>}
    </div>
  )
}

function Label({ children }: { children: ReactNode }) {
  return <div className="text-[10px] uppercase tracking-wide text-gray-400 dark:text-gray-500 mb-0.5">{children}</div>
}

// Code is a monospace block on the code surface; long content gets height-locked.
function Code({ text, cap = 200 }: { text: string; cap?: number }) {
  return (
    <Expandable maxHeight={cap} fade="from-gray-50 dark:from-gray-900">
      <pre className="bg-gray-50 dark:bg-gray-900 rounded p-2 overflow-x-auto whitespace-pre-wrap font-mono text-[11px] text-gray-700 dark:text-gray-200">{text}</pre>
    </Expandable>
  )
}

// DiffView renders classified diff lines with a gutter marker and add/remove
// colouring. Shared by edit_file (LCS of old→new) and git_diff (a unified diff).
function DiffView({ lines, cap = 280 }: { lines: DiffLine[]; cap?: number }) {
  return (
    <Expandable maxHeight={cap} fade="from-gray-50 dark:from-gray-900">
      <div className="bg-gray-50 dark:bg-gray-900 rounded overflow-x-auto font-mono text-[11px] leading-relaxed">
        {lines.map((l, i) => (
          <div key={i} className={lineClass(l.type)}>
            <span className="select-none inline-block w-3 text-center opacity-60">{marker(l.type)}</span>
            <span className="whitespace-pre-wrap break-all">{l.text}</span>
          </div>
        ))}
      </div>
    </Expandable>
  )
}

function marker(t: DiffLine['type']): string {
  return t === 'add' ? '+' : t === 'remove' ? '-' : ' '
}

function lineClass(t: DiffLine['type']): string {
  switch (t) {
    case 'add': return 'px-2 bg-green-50 dark:bg-green-900/30 text-green-800 dark:text-green-300'
    case 'remove': return 'px-2 bg-red-50 dark:bg-red-900/30 text-red-800 dark:text-red-300'
    case 'meta': return 'px-2 text-gray-400 dark:text-gray-500'
    default: return 'px-2 text-gray-600 dark:text-gray-300'
  }
}

// isFlatRecord reports whether v is a plain object whose values are all
// primitives (or absent) — the shape KeyValueBlock can render as a scannable
// key→value list instead of a pretty-printed JSON block.
function isFlatRecord(v: unknown): v is Record<string, unknown> {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return false
  return Object.values(v as Record<string, unknown>).every(x => x == null || typeof x !== 'object')
}

// KeyValueBlock renders a flat object as a compact key→value list — the
// fallback's preferred shape (#404) when every field is a primitive, e.g. an
// ACP tool call whose args are just `{title: "..."}`.
function KeyValueBlock({ data }: { data: Record<string, unknown> }) {
  const entries = Object.entries(data).filter(([, v]) => v !== undefined && v !== '')
  if (entries.length === 0) return <span className="text-[11px] text-gray-400 dark:text-gray-500">(empty)</span>
  return (
    <div className="bg-gray-50 dark:bg-gray-900 rounded p-2 text-[11px] space-y-0.5">
      {entries.map(([k, v]) => (
        <div key={k} className="flex gap-2">
          <span className="text-gray-400 dark:text-gray-500 shrink-0">{k}</span>
          <span className="text-gray-700 dark:text-gray-200 font-mono break-all">{String(v)}</span>
        </div>
      ))}
    </div>
  )
}

// FormattedValue is the fallback's per-value renderer: a compact key→value
// list for a flat object, else a pretty-printed (never raw single-line) JSON
// block — either way, never a raw dump (#404).
function FormattedValue({ value }: { value: unknown }) {
  if (isFlatRecord(value)) return <KeyValueBlock data={value} />
  return <Code text={prettyJSON(value)} cap={160} />
}

// Result renders a tool's result: a message when it succeeded silently, else a
// tidy formatted (never raw-blob) dump. Callers that show a richer result skip it.
function ResultJSON({ result }: { result: unknown }) {
  return (
    <div>
      <Label>result</Label>
      <FormattedValue value={result} />
    </div>
  )
}

// ── per-tool views ─────────────────────────────────────────────────────────────

// EditFileView — the flagship: renders the targeted replacement as a before→after
// diff (old lines red, new lines green), headed by the file path. `replace_all`
// and the applied-replacements count (from the result) surface as badges.
function EditFileView({ tool }: { tool: ToolCall }) {
  const path = str(tool.args, 'path') ?? '(unknown path)'
  const oldStr = str(tool.args, 'old') ?? ''
  const newStr = str(tool.args, 'new') ?? ''
  const replaceAll = bool(tool.args, 'replace_all')
  const replacements = tool.done ? num(tool.result, 'replacements') : undefined
  return (
    <div className="space-y-1">
      <PathHeader
        path={path}
        note={
          <>
            {replaceAll && <span className="mr-1">replace all</span>}
            {replacements != null && <span>{replacements} replacement{replacements === 1 ? '' : 's'}</span>}
          </>
        }
      />
      <DiffView lines={lineDiff(oldStr, newStr)} />
    </div>
  )
}

// WriteFileView — path header + collapsible content; result reports created/overwrote.
function WriteFileView({ tool }: { tool: ToolCall }) {
  const path = str(tool.args, 'path') ?? '(unknown path)'
  const content = str(tool.args, 'content') ?? ''
  const created = tool.done ? bool(tool.result, 'created') : undefined
  return (
    <div className="space-y-1">
      <PathHeader path={path} note={created == null ? undefined : created ? 'created' : 'overwrote'} />
      <Code text={content} cap={200} />
    </div>
  )
}

// ReadFileView — path (+ line window) header + a snippet of the file content.
function ReadFileView({ tool }: { tool: ToolCall }) {
  const path = str(tool.args, 'path') ?? '(unknown path)'
  const offset = num(tool.args, 'offset')
  const limit = num(tool.args, 'limit')
  const window = offset || limit ? `lines ${offset ?? 0}${limit ? `–${(offset ?? 0) + limit}` : '+'}` : undefined
  const content = tool.done ? str(tool.result, 'content') : undefined
  const truncated = tool.done ? bool(tool.result, 'truncated') : undefined
  return (
    <div className="space-y-1">
      <PathHeader path={path} note={window} />
      {content != null
        ? <Code text={truncated ? content + '\n…(truncated)' : content} cap={200} />
        : !tool.done && <span className="text-[11px] text-gray-400 dark:text-gray-500">reading…</span>}
    </div>
  )
}

// RunCommandView — the command as a shell line + its combined output; exit code badge.
function RunCommandView({ tool }: { tool: ToolCall }) {
  const command = str(tool.args, 'command') ?? ''
  const exit = tool.done ? num(tool.result, 'exit_code') : undefined
  const output = tool.done ? str(tool.result, 'output') : undefined
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2">
        <code className="font-mono text-[11px] text-gray-700 dark:text-gray-200 break-all">
          <span className="text-gray-400 dark:text-gray-500 select-none">$ </span>{command}
        </code>
        {exit != null && (
          <span className={`text-[10px] font-medium ${exit === 0 ? 'text-green-600 dark:text-green-400' : 'text-red-500 dark:text-red-400'}`}>
            exit {exit}
          </span>
        )}
      </div>
      {output != null && output !== '' && <Code text={output} cap={200} />}
    </div>
  )
}

// GitCommitView — commit message + files-changed count + short SHA.
function GitCommitView({ tool }: { tool: ToolCall }) {
  const message = str(tool.args, 'message') ?? ''
  const sha = tool.done ? str(tool.result, 'sha') : undefined
  const files = tool.done ? num(tool.result, 'files_changed') : undefined
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2 text-[11px]">
        {sha && <code className="font-mono text-amber-600 dark:text-amber-400">{sha.slice(0, 7)}</code>}
        {files != null && <span className="text-gray-500 dark:text-gray-400">{files} file{files === 1 ? '' : 's'} changed</span>}
      </div>
      <div className="text-[11px] text-gray-700 dark:text-gray-200 whitespace-pre-wrap font-mono bg-gray-50 dark:bg-gray-900 rounded p-2">{message}</div>
    </div>
  )
}

// GitDiffView — the unified-diff result rendered coloured, headed by its target.
function GitDiffView({ tool }: { tool: ToolCall }) {
  const ref = str(tool.args, 'ref')
  const path = str(tool.args, 'path')
  const diff = tool.done ? str(tool.result, 'diff') : undefined
  const truncated = tool.done ? bool(tool.result, 'truncated') : undefined
  const target = [ref, path].filter(Boolean).join(' · ')
  return (
    <div className="space-y-1">
      {target && <PathHeader path={target} note={truncated ? 'truncated' : undefined} />}
      {diff != null && diff !== ''
        ? <DiffView lines={parseUnifiedDiff(diff)} />
        : tool.done && <span className="text-[11px] text-gray-400 dark:text-gray-500">no changes</span>}
    </div>
  )
}

// GitLogView — a compact commit list (short SHA · subject · author · date).
function GitLogView({ tool }: { tool: ToolCall }) {
  const commits = tool.done && tool.result && typeof tool.result === 'object'
    ? (tool.result as { commits?: unknown }).commits
    : undefined
  if (!Array.isArray(commits)) return <GenericView tool={tool} />
  return (
    <Expandable maxHeight={220} fade="from-gray-50 dark:from-gray-900">
      <ul className="bg-gray-50 dark:bg-gray-900 rounded p-2 space-y-1 text-[11px]">
        {commits.map((c, i) => (
          <li key={i} className="flex gap-2">
            <code className="font-mono text-amber-600 dark:text-amber-400 shrink-0">{(str(c, 'sha') ?? '').slice(0, 7)}</code>
            <span className="text-gray-700 dark:text-gray-200 truncate">{str(c, 'subject')}</span>
            <span className="ml-auto shrink-0 text-gray-400 dark:text-gray-500">{str(c, 'author')}</span>
          </li>
        ))}
      </ul>
    </Expandable>
  )
}

// ── unmapped ACP / native tools ─────────────────────────────────────────────────

// ListDirView — the directory's entries (name, dir/file marker, size), capped
// like the other list views; a `truncated` result note surfaces alongside cwd.
function ListDirView({ tool }: { tool: ToolCall }) {
  const path = str(tool.args, 'path') || '.'
  const entries = tool.done && tool.result && typeof tool.result === 'object'
    ? (tool.result as { entries?: unknown }).entries
    : undefined
  const truncated = tool.done ? bool(tool.result, 'truncated') : undefined
  if (!Array.isArray(entries)) return <GenericView tool={tool} />
  return (
    <div className="space-y-1">
      <PathHeader path={path} note={truncated ? 'truncated' : undefined} />
      <Expandable maxHeight={220} fade="from-gray-50 dark:from-gray-900">
        <ul className="bg-gray-50 dark:bg-gray-900 rounded p-2 text-[11px] font-mono">
          {entries.map((e, i) => (
            <li key={i} className="text-gray-700 dark:text-gray-200 truncate">
              <span className="text-gray-400 dark:text-gray-500 mr-1">{bool(e, 'dir') ? '📁' : ' '}</span>
              {str(e, 'path')}
            </li>
          ))}
        </ul>
      </Expandable>
    </div>
  )
}

// GlobView — the matched paths as a plain list, headed by the pattern.
function GlobView({ tool }: { tool: ToolCall }) {
  const pattern = str(tool.args, 'pattern') ?? ''
  const paths = tool.done && tool.result && typeof tool.result === 'object'
    ? (tool.result as { paths?: unknown }).paths
    : undefined
  const truncated = tool.done ? bool(tool.result, 'truncated') : undefined
  if (!Array.isArray(paths)) return <GenericView tool={tool} />
  return (
    <div className="space-y-1">
      <PathHeader path={pattern} note={truncated ? 'truncated' : undefined} />
      <Expandable maxHeight={220} fade="from-gray-50 dark:from-gray-900">
        <ul className="bg-gray-50 dark:bg-gray-900 rounded p-2 text-[11px] font-mono">
          {paths.map((p, i) => <li key={i} className="text-gray-700 dark:text-gray-200 truncate">{String(p)}</li>)}
        </ul>
      </Expandable>
    </div>
  )
}

// GrepView — matches as `path:line  text`, headed by the pattern (+ glob filter).
function GrepView({ tool }: { tool: ToolCall }) {
  const pattern = str(tool.args, 'pattern') ?? ''
  const glob = str(tool.args, 'glob')
  const matches = tool.done && tool.result && typeof tool.result === 'object'
    ? (tool.result as { matches?: unknown }).matches
    : undefined
  const truncated = tool.done ? bool(tool.result, 'truncated') : undefined
  if (!Array.isArray(matches)) return <GenericView tool={tool} />
  return (
    <div className="space-y-1">
      <PathHeader path={pattern} note={<>{glob && <span className="mr-1">{glob}</span>}{truncated && 'truncated'}</>} />
      <Expandable maxHeight={220} fade="from-gray-50 dark:from-gray-900">
        <ul className="bg-gray-50 dark:bg-gray-900 rounded p-2 text-[11px] font-mono space-y-0.5">
          {matches.map((m, i) => (
            <li key={i} className="flex gap-2 text-gray-700 dark:text-gray-200">
              <span className="text-amber-600 dark:text-amber-400 shrink-0">{str(m, 'path')}:{num(m, 'line')}</span>
              <span className="truncate">{str(m, 'text')}</span>
            </li>
          ))}
        </ul>
      </Expandable>
    </div>
  )
}

// WebSearchView — each result as a title/url/snippet card.
function WebSearchView({ tool }: { tool: ToolCall }) {
  const query = str(tool.args, 'query') ?? ''
  const results = tool.done && tool.result && typeof tool.result === 'object'
    ? (tool.result as { results?: unknown }).results
    : undefined
  return (
    <div className="space-y-1">
      <PathHeader path={`"${query}"`} />
      {Array.isArray(results) ? (
        <Expandable maxHeight={260} fade="from-gray-50 dark:from-gray-900">
          <ul className="space-y-1.5">
            {results.map((r, i) => (
              <li key={i} className="bg-gray-50 dark:bg-gray-900 rounded p-2 text-[11px]">
                <div className="text-gray-700 dark:text-gray-200 font-medium truncate">{str(r, 'title')}</div>
                <div className="text-blue-600 dark:text-blue-400 truncate">{str(r, 'url')}</div>
                {str(r, 'snippet') && <div className="text-gray-500 dark:text-gray-400 mt-0.5">{str(r, 'snippet')}</div>}
              </li>
            ))}
          </ul>
        </Expandable>
      ) : !tool.done && <span className="text-[11px] text-gray-400 dark:text-gray-500">searching…</span>}
    </div>
  )
}

// WebFetchView — url header + the fetched page text (a plain string result).
function WebFetchView({ tool }: { tool: ToolCall }) {
  const url = str(tool.args, 'url') ?? ''
  const pattern = str(tool.args, 'pattern')
  const offset = num(tool.args, 'offset')
  const note = pattern ? `pattern: ${pattern}` : offset ? `from line ${offset}` : undefined
  const text = tool.done && typeof tool.result === 'string' ? tool.result : undefined
  return (
    <div className="space-y-1">
      <PathHeader path={url} note={note} />
      {text != null
        ? <Code text={text} cap={220} />
        : !tool.done && <span className="text-[11px] text-gray-400 dark:text-gray-500">fetching…</span>}
    </div>
  )
}

// AskAdvisorView — the worker's request followed by the advisor's reply, both
// prose (not code), since neither is source text.
function AskAdvisorView({ tool }: { tool: ToolCall }) {
  const request = str(tool.args, 'request') ?? ''
  const advice = tool.done ? str(tool.result, 'advice') : undefined
  return (
    <div className="space-y-1">
      <div>
        <Label>asked</Label>
        <div className="text-[11px] text-gray-700 dark:text-gray-200 bg-gray-50 dark:bg-gray-900 rounded p-2 whitespace-pre-wrap">{request}</div>
      </div>
      {advice != null && (
        <div>
          <Label>advice</Label>
          <Expandable maxHeight={200} fade="from-gray-50 dark:from-gray-900">
            <div className="text-[11px] text-gray-700 dark:text-gray-200 bg-gray-50 dark:bg-gray-900 rounded p-2 whitespace-pre-wrap">{advice}</div>
          </Expandable>
        </div>
      )}
    </div>
  )
}

// StageMemoryView — the staged fact + its bucket; the result is a short status
// string ("kept only if the answer passes vetting"), not worth its own label.
function StageMemoryView({ tool }: { tool: ToolCall }) {
  const content = str(tool.args, 'content') ?? ''
  const kind = str(tool.args, 'kind')
  const bucket = str(tool.args, 'bucket')
  const status = tool.done && typeof tool.result === 'string' ? tool.result : undefined
  return (
    <div className="space-y-1">
      <PathHeader path={[bucket, kind].filter(Boolean).join(' · ') || 'memory'} />
      <div className="text-[11px] text-gray-700 dark:text-gray-200 bg-gray-50 dark:bg-gray-900 rounded p-2 whitespace-pre-wrap">{content}</div>
      {status && <div className="text-[10px] text-gray-400 dark:text-gray-500 italic">{status}</div>}
    </div>
  )
}

// LoadMemoryView — recalled memory entries as short prose snippets. ADK's
// native result shape (`memories: [{content: {parts: [{text}]}, author}]`) is
// read defensively — memoryText tries a few known shapes and falls back to
// FormattedValue rather than assume the exact schema.
function memoryText(entry: unknown): string {
  const content = entry && typeof entry === 'object' ? (entry as Record<string, unknown>).content : undefined
  const parts = content && typeof content === 'object' ? (content as Record<string, unknown>).parts : undefined
  if (Array.isArray(parts)) {
    const text = parts.map(p => str(p, 'text') ?? '').join(' ').trim()
    if (text) return text
  }
  return str(entry, 'text') ?? ''
}

function LoadMemoryView({ tool }: { tool: ToolCall }) {
  const query = str(tool.args, 'query') ?? ''
  const memories = tool.done && tool.result && typeof tool.result === 'object'
    ? (tool.result as { memories?: unknown }).memories
    : undefined
  if (!Array.isArray(memories)) {
    return (
      <div className="space-y-1">
        <PathHeader path={`"${query}"`} />
        {tool.done && <FormattedValue value={tool.result} />}
      </div>
    )
  }
  return (
    <div className="space-y-1">
      <PathHeader path={`"${query}"`} note={memories.length === 0 ? 'no matches' : undefined} />
      {memories.length > 0 && (
        <Expandable maxHeight={220} fade="from-gray-50 dark:from-gray-900">
          <ul className="space-y-1">
            {memories.map((m, i) => {
              const text = memoryText(m)
              return text ? (
                <li key={i} className="bg-gray-50 dark:bg-gray-900 rounded p-2 text-[11px] text-gray-700 dark:text-gray-200">{text}</li>
              ) : null
            })}
          </ul>
        </Expandable>
      )}
    </div>
  )
}

// GetUserChoiceView — the question + its options; the chosen one (once
// answered) is highlighted, and a still-open call shows the pending badge.
function GetUserChoiceView({ tool }: { tool: ToolCall }) {
  const question = str(tool.args, 'question') ?? ''
  const options = Array.isArray(tool.args.options) ? tool.args.options as unknown[] : []
  const status = tool.done ? str(tool.result, 'status') : undefined
  const choice = tool.done ? str(tool.result, 'choice') : undefined
  return (
    <div className="space-y-1">
      <div className="text-[11px] text-gray-700 dark:text-gray-200 font-medium">{question}</div>
      <ul className="space-y-0.5">
        {options.map((o, i) => {
          const opt = String(o)
          const chosen = choice != null && opt === choice
          return (
            <li key={i} className={`text-[11px] rounded px-2 py-1 ${chosen ? 'bg-green-50 dark:bg-green-900/30 text-green-800 dark:text-green-300 font-medium' : 'bg-gray-50 dark:bg-gray-900 text-gray-600 dark:text-gray-300'}`}>
              {chosen && '✓ '}{opt}
            </li>
          )
        })}
      </ul>
      {status === 'pending' && <div className="text-[10px] text-amber-600 dark:text-amber-400 italic">awaiting your answer…</div>}
    </div>
  )
}

// AskUserView — the mid-node clarifying question; a pause/resume marker.
function AskUserView({ tool }: { tool: ToolCall }) {
  const question = str(tool.args, 'question') ?? ''
  const status = tool.done ? str(tool.result, 'status') : undefined
  return (
    <div className="space-y-1">
      <div className="text-[11px] text-gray-700 dark:text-gray-200 font-medium">{question}</div>
      {status && <div className="text-[10px] text-gray-400 dark:text-gray-500 italic">{status}</div>}
    </div>
  )
}

// DeletePathView — the removed path + whether it existed.
function DeletePathView({ tool }: { tool: ToolCall }) {
  const path = str(tool.args, 'path') ?? '(unknown path)'
  const deleted = tool.done ? bool(tool.result, 'deleted') : undefined
  return <PathHeader path={path} note={deleted == null ? undefined : deleted ? 'deleted' : 'not found'} />
}

// GitStatusView / GitBranchView / GitPushView — the remaining ledger-known git
// ops (git.go / vetting/ledger.go): compact one-line summaries, not a blob.
function GitStatusView({ tool }: { tool: ToolCall }) {
  const branch = tool.done ? str(tool.result, 'branch') : undefined
  const clean = tool.done ? bool(tool.result, 'clean') : undefined
  return (
    <div className="text-[11px] flex items-center gap-2">
      {branch && <code className="font-mono text-gray-700 dark:text-gray-200">{branch}</code>}
      {clean != null && (
        <span className={clean ? 'text-green-600 dark:text-green-400' : 'text-amber-600 dark:text-amber-400'}>
          {clean ? 'clean' : 'dirty'}
        </span>
      )}
    </div>
  )
}

function GitBranchView({ tool }: { tool: ToolCall }) {
  const name = str(tool.args, 'name')
  const current = tool.done ? str(tool.result, 'current') : undefined
  return <code className="font-mono text-[11px] text-gray-700 dark:text-gray-200">{current ?? name ?? ''}</code>
}

function GitPushView({ tool }: { tool: ToolCall }) {
  const branch = tool.done ? str(tool.result, 'branch') : undefined
  const sha = tool.done ? str(tool.result, 'sha') : undefined
  return (
    <div className="text-[11px] flex items-center gap-2">
      {branch && <code className="font-mono text-gray-700 dark:text-gray-200">{branch}</code>}
      {sha && <code className="font-mono text-amber-600 dark:text-amber-400">{sha.slice(0, 7)}</code>}
    </div>
  )
}

// GenericView — the tidy fallback for tools without a custom view: a compact
// key→value list when args/result are flat, else pretty (never raw-blob) JSON —
// either way height-locked so an unfamiliar tool can't wall off the node.
function GenericView({ tool }: { tool: ToolCall }) {
  return (
    <div className="space-y-2">
      <div>
        <Label>args</Label>
        <FormattedValue value={tool.args} />
      </div>
      {tool.done && <ResultJSON result={tool.result} />}
    </div>
  )
}
