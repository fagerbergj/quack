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
export function ToolCallView({ tool }: { tool: ToolCall }) {
  switch (tool.name) {
    case 'edit_file': return <EditFileView tool={tool} />
    case 'write_file': return <WriteFileView tool={tool} />
    case 'read_file': return <ReadFileView tool={tool} />
    case 'run_command': return <RunCommandView tool={tool} />
    case 'git_commit': return <GitCommitView tool={tool} />
    case 'git_diff': return <GitDiffView tool={tool} />
    case 'git_log': return <GitLogView tool={tool} />
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

// Result renders a tool's result: a message when it succeeded silently, else a
// tidy formatted (never raw-blob) dump. Callers that show a richer result skip it.
function ResultJSON({ result }: { result: unknown }) {
  return (
    <div>
      <Label>result</Label>
      <Code text={prettyJSON(result)} cap={160} />
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

// GenericView — the tidy fallback for tools without a custom view: pretty (not
// raw-blob) args + result, each height-locked.
function GenericView({ tool }: { tool: ToolCall }) {
  return (
    <div className="space-y-2">
      <div>
        <Label>args</Label>
        <Code text={prettyJSON(tool.args)} cap={200} />
      </div>
      {tool.done && <ResultJSON result={tool.result} />}
    </div>
  )
}
