import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { ToolCallView } from './ToolCallView'
import type { ToolCall } from './messageParts'

// Structural assertions on the static markup — no testing-library in this repo, so
// we render each tool view to HTML and check the load-bearing shape (a diff for
// edit_file, a formatted-not-raw fallback, etc.). Effects don't run under
// renderToStaticMarkup, which is fine: these views have no measured state.
function html(tool: ToolCall): string {
  return renderToStaticMarkup(createElement(ToolCallView, { tool }))
}

describe('ToolCallView — edit_file diff (flagship)', () => {
  const tool: ToolCall = {
    callId: 'c1', name: 'edit_file', done: true,
    args: { path: 'src/app.ts', old: 'const x = 1', new: 'const x = 2' },
    result: { replacements: 1 },
  }
  const out = html(tool)

  it('headers the file path', () => {
    expect(out).toContain('src/app.ts')
  })
  it('shows the removed line in red and the added line in green', () => {
    expect(out).toContain('const x = 1')
    expect(out).toContain('const x = 2')
    expect(out).toMatch(/bg-red-50[^"]*">[^<]*<span[^>]*>-<\/span>/) // remove gutter
    expect(out).toMatch(/bg-green-50[^"]*">[^<]*<span[^>]*>\+<\/span>/) // add gutter
  })
  it('surfaces the applied replacement count', () => {
    expect(out).toContain('1 replacement')
  })
})

describe('ToolCallView — other common tools', () => {
  it('run_command shows the shell command and its exit code', () => {
    const out = html({
      callId: 'c', name: 'run_command', done: true,
      args: { dir: '/w', command: 'go test ./...' },
      result: { exit_code: 0, output: 'ok', duration_ms: 12 },
    })
    expect(out).toContain('go test ./...')
    expect(out).toContain('exit 0')
    expect(out).toContain('$ ')
  })

  it('git_commit shows the message and files-changed count', () => {
    const out = html({
      callId: 'c', name: 'git_commit', done: true,
      args: { dir: '/w', message: 'fix: thing' },
      result: { sha: 'abcdef1234', files_changed: 3 },
    })
    expect(out).toContain('fix: thing')
    expect(out).toContain('3 files changed')
    expect(out).toContain('abcdef1') // short sha
  })

  it('write_file headers the path and reports created', () => {
    const out = html({
      callId: 'c', name: 'write_file', done: true,
      args: { path: 'a/b.txt', content: 'hello' },
      result: { created: true },
    })
    expect(out).toContain('a/b.txt')
    expect(out).toContain('created')
  })

  it('git_diff renders the unified diff coloured, not as a raw blob', () => {
    const out = html({
      callId: 'c', name: 'git_diff', done: true,
      args: { dir: '/w' },
      result: { diff: '@@ -1 +1 @@\n-old\n+new', truncated: false },
    })
    expect(out).toContain('bg-green-50') // added line styled
    expect(out).toContain('bg-red-50') // removed line styled
  })
})

describe('ToolCallView — fallback', () => {
  it('renders an unknown tool with nested args as tidy pretty JSON, not a raw dump', () => {
    const out = html({
      callId: 'c', name: 'mystery_tool', done: true,
      args: { target: { region: 'eu', tags: ['a', 'b'] } }, result: { ok: true },
    })
    // formatted args + result labels, pretty JSON (indented) — never a bare dump.
    // Quotes are HTML-escaped in static markup (&quot;).
    expect(out).toContain('args')
    expect(out).toContain('result')
    expect(out).toContain('&quot;region&quot;: &quot;eu&quot;')
  })

  it('renders an unknown tool with FLAT args as a compact key→value list', () => {
    const out = html({
      callId: 'c', name: 'mystery_flat_tool', done: true,
      args: { title: 'Some ACP tool title' },
    })
    expect(out).toContain('title')
    expect(out).toContain('Some ACP tool title')
    // Not a JSON blob: no braces/quoted-key punctuation in the args block.
    expect(out).not.toContain('&quot;title&quot;:')
  })
})

describe('ToolCallView — new per-tool views (#404)', () => {
  it('web_search shows the query and result cards, not raw JSON', () => {
    const out = html({
      callId: 'c', name: 'web_search', done: true,
      args: { query: 'best time to visit Dublin' },
      result: { results: [{ title: 'Dublin Climate', url: 'https://example.com/climate', snippet: 'Mild year-round.' }] },
    })
    expect(out).toContain('best time to visit Dublin')
    expect(out).toContain('Dublin Climate')
    expect(out).toContain('https://example.com/climate')
    expect(out).not.toContain('&quot;results&quot;:')
  })

  it('web_fetch shows the url and the fetched text (a plain string result)', () => {
    const out = html({
      callId: 'c', name: 'web_fetch', done: true,
      args: { url: 'https://example.com/climate' },
      result: 'Dublin is mild year-round.',
    })
    expect(out).toContain('https://example.com/climate')
    expect(out).toContain('Dublin is mild year-round.')
  })

  it('grep shows path:line matches, not a raw dump', () => {
    const out = html({
      callId: 'c', name: 'grep', done: true,
      args: { pattern: 'TODO' },
      result: { matches: [{ path: 'a.go', line: 12, text: '// TODO: fix' }], truncated: false },
    })
    expect(out).toContain('a.go')
    expect(out).toContain('12')
    expect(out).toContain('// TODO: fix')
  })

  it('get_user_choice highlights the chosen option once answered', () => {
    const out = html({
      callId: 'c', name: 'get_user_choice', done: true,
      args: { question: 'Which Springfield?', options: ['Springfield, IL', 'Springfield, MA'] },
      result: { status: 'answered', choice: 'Springfield, IL' },
    })
    expect(out).toContain('Which Springfield?')
    expect(out).toContain('Springfield, IL')
    expect(out).toContain('✓')
  })

  it('get_user_choice still pending shows the awaiting marker', () => {
    const out = html({
      callId: 'c', name: 'get_user_choice', done: true,
      args: { question: 'Which Springfield?', options: ['A', 'B'] },
      result: { status: 'pending' },
    })
    expect(out).toContain('awaiting your answer')
  })
})
