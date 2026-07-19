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

// Unknown tool, nested args → tidy pretty-printed JSON fallback (never a raw blob).
export const FallbackNested: Story = render({
  callId: 'c', name: 'mystery_tool', done: true,
  args: { target: { region: 'eu-west', tags: ['prod', 'db'] } },
  result: { ok: true, note: 'no custom view for this tool yet' },
})

// Unknown tool, FLAT args (e.g. an ACP tool kind we don't specially map) →
// a compact key→value list instead of pretty JSON.
export const FallbackFlat: Story = render({
  callId: 'c', name: 'move', done: true,
  args: { title: 'rename src/old.ts → src/new.ts' },
  result: { moved: true },
})

// A call still running: args show, result is pending.
export const Running: Story = render({
  callId: 'c', name: 'run_command', done: false,
  args: { dir: '.', command: 'npm run build' },
})

// ── the tools added by #404 ─────────────────────────────────────────────────

export const WebSearch: Story = render({
  callId: 'c', name: 'web_search', done: true,
  args: { query: 'best time to visit Dublin' },
  result: {
    results: [
      { title: 'Dublin Climate Guide', url: 'https://example.com/climate', snippet: 'Mild year-round; May–September is warmest.' },
      { title: 'Best Time to Visit Ireland', url: 'https://example.com/ireland', snippet: 'Shoulder seasons avoid the summer crowds.' },
    ],
  },
})

export const WebFetch: Story = render({
  callId: 'c', name: 'web_fetch', done: true,
  args: { url: 'https://example.com/climate' },
  result: 'Dublin has a temperate maritime climate. Summers are mild, winters cool and damp.',
})

export const ListDir: Story = render({
  callId: 'c', name: 'list_dir', done: true,
  args: { path: 'internal/dag', depth: 1 },
  result: { entries: [{ path: 'internal/dag/plan.go', dir: false, size: 1024 }, { path: 'internal/dag/planner.go', dir: false, size: 2048 }, { path: 'internal/dag/graph_test.go', dir: false, size: 512 }], truncated: false, cwd: '.' },
})

export const Glob: Story = render({
  callId: 'c', name: 'glob', done: true,
  args: { pattern: '**/*_test.go', path: 'internal/dag' },
  result: { paths: ['internal/dag/plan_test.go', 'internal/dag/graph_test.go'], truncated: false, cwd: '.' },
})

export const Grep: Story = render({
  callId: 'c', name: 'grep', done: true,
  args: { pattern: 'TODO', glob: '*.go' },
  result: { matches: [{ path: 'internal/dag/plan.go', line: 42, text: '// TODO: validate cycles' }], truncated: false, cwd: '.' },
})

export const AskAdvisor: Story = render({
  callId: 'c', name: 'ask_advisor', done: true,
  args: { request: "I'm about to fix the debounce timer leak — should I clear it in a cleanup or add a guard flag?" },
  result: { advice: 'A cleanup function is the idiomatic React pattern — a guard flag is easy to forget on every new effect.' },
})

export const StageMemory: Story = render({
  callId: 'c', name: 'stage_memory', done: true,
  args: { content: 'This repo runs `go test ./...` before every commit.', kind: 'convention', bucket: 'repo' },
  result: 'Staged for memory (kept only if the answer passes vetting).',
})

export const LoadMemory: Story = render({
  callId: 'c', name: 'load_memory', done: true,
  args: { query: 'repo build commands' },
  result: { memories: [{ content: { parts: [{ text: 'Build with `make build`; test with `go test ./...`.' }] }, author: 'repo' }] },
})

export const GetUserChoicePending: Story = render({
  callId: 'c', name: 'get_user_choice', done: true,
  args: { question: 'Which Springfield do you mean?', options: ['Springfield, IL', 'Springfield, MA', 'Springfield, OR'] },
  result: { status: 'pending' },
})

export const GetUserChoiceAnswered: Story = render({
  callId: 'c', name: 'get_user_choice', done: true,
  args: { question: 'Which Springfield do you mean?', options: ['Springfield, IL', 'Springfield, MA'] },
  result: { status: 'answered', choice: 'Springfield, IL' },
})

export const AskUser: Story = render({
  callId: 'c', name: 'ask_user', done: true,
  args: { question: 'Do you want the migration to run against staging first?' },
  result: { status: 'forwarded to the user; their answer will arrive in a follow-up' },
})

export const GitStatus: Story = render({
  callId: 'c', name: 'git_status', done: true,
  args: { dir: '.' },
  result: { branch: 'feat/404-tool-rendering', clean: false },
})

export const GitBranch: Story = render({
  callId: 'c', name: 'git_branch', done: true,
  args: { dir: '.', name: 'feat/404-tool-rendering' },
  result: { current: 'feat/404-tool-rendering' },
})

export const GitPush: Story = render({
  callId: 'c', name: 'git_push', done: true,
  args: { dir: '.' },
  result: { remote: 'origin', branch: 'feat/404-tool-rendering', sha: 'a1b2c3d4e5f6' },
})
