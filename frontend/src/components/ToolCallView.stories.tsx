import type { Meta, StoryObj } from '@storybook/react-vite'
import { ToolCallView } from './ToolCallView'
import type { ToolCall } from './messageParts'

const meta: Meta<typeof ToolCallView> = {
  title: 'Chat/ToolCallView',
  component: ToolCallView,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof ToolCallView>

const render = (tool: ToolCall) => ({
  render: () => (
    <div className="max-w-2xl rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <ToolCallView tool={tool} />
    </div>
  ),
})

// ── edit_file: the flagship diff ────────────────────────────────────────────

const oldFn = `func greet(name string) string {
\treturn "Hello, " + name
}`
const newFn = `func greet(name string, loud bool) string {
\tg := "Hello, " + name
\tif loud {
\t\tg += "!"
\t}
\treturn g
}`

export const EditFileDiff: Story = render({
  callId: 'c1', name: 'edit_file', done: true,
  args: { path: 'internal/greet/greet.go', old: oldFn, new: newFn },
  result: { replacements: 1 },
})

export const EditFileReplaceAll: Story = render({
  callId: 'c1', name: 'edit_file', done: true,
  args: { path: 'src/config.ts', old: 'oldName', new: 'newName', replace_all: true },
  result: { replacements: 4 },
})

// A long edit stays height-locked (Show more) rather than walling off the node.
export const EditFileLong: Story = render({
  callId: 'c1', name: 'edit_file', done: true,
  args: {
    path: 'src/big.ts',
    old: Array.from({ length: 30 }, (_, i) => `const before_${i} = ${i}`).join('\n'),
    new: Array.from({ length: 30 }, (_, i) => `const after_${i} = ${i * 2}`).join('\n'),
  },
  result: { replacements: 1 },
})

// ── other tool views ────────────────────────────────────────────────────────

export const WriteFile: Story = render({
  callId: 'c', name: 'write_file', done: true,
  args: { path: 'docs/notes.md', content: '# Notes\n\n- first\n- second\n- third' },
  result: { created: true },
})

export const ReadFile: Story = render({
  callId: 'c', name: 'read_file', done: true,
  args: { path: 'go.mod', offset: 0, limit: 10 },
  result: { content: 'module github.com/fagerbergj/quack\n\ngo 1.24', truncated: false, total_lines: 3 },
})

export const RunCommandOk: Story = render({
  callId: 'c', name: 'run_command', done: true,
  args: { dir: '.', command: 'go test ./...' },
  result: { exit_code: 0, output: 'ok  \tgithub.com/fagerbergj/quack/internal/dag\t0.312s', duration_ms: 340 },
})

export const RunCommandFail: Story = render({
  callId: 'c', name: 'run_command', done: true,
  args: { dir: '.', command: 'go build ./...' },
  result: { exit_code: 1, output: 'internal/dag/plan.go:12:2: undefined: Foo', duration_ms: 120 },
})

export const GitCommit: Story = render({
  callId: 'c', name: 'git_commit', done: true,
  args: { dir: '.', message: 'feat(dag): expandable long content + rich tool views' },
  result: { sha: 'a1b2c3d4e5f6', files_changed: 5 },
})

export const GitDiff: Story = render({
  callId: 'c', name: 'git_diff', done: true,
  args: { dir: '.', ref: 'HEAD', path: 'plan.go' },
  result: {
    diff: [
      'diff --git a/plan.go b/plan.go',
      '--- a/plan.go',
      '+++ b/plan.go',
      '@@ -1,3 +1,3 @@',
      ' package dag',
      '-type Node struct{}',
      '+type Node struct{ ID string }',
    ].join('\n'),
    truncated: false,
  },
})

export const GitLog: Story = render({
  callId: 'c', name: 'git_log', done: true,
  args: { dir: '.', n: 3 },
  result: {
    commits: [
      { sha: 'a1b2c3d4', author: 'Jason', date: '2026-07-12', subject: 'feat: expandable DAG content' },
      { sha: 'b2c3d4e5', author: 'Jason', date: '2026-07-11', subject: 'fix: judge round budget' },
      { sha: 'c3d4e5f6', author: 'Jason', date: '2026-07-10', subject: 'chore: bump deps' },
    ],
  },
})

// Unknown tool → tidy formatted fallback (pretty args + result, never a raw blob).
export const Fallback: Story = render({
  callId: 'c', name: 'web_search', done: true,
  args: { query: 'best time to visit Dublin', max_results: 5 },
  result: { results: [{ title: 'Dublin Climate', url: 'https://example.com' }] },
})

// A call still running: args show, result is pending.
export const Running: Story = render({
  callId: 'c', name: 'run_command', done: false,
  args: { dir: '.', command: 'npm run build' },
})
