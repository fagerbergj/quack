import { describe, it, expect } from 'vitest'
import { page } from 'vitest/browser'
import { render, cleanup } from '@testing-library/react'
import { composeStories } from '@storybook/react-vite'

// #1192: two quack-authored PRs passed every gate (tsc/eslint/424 RTL
// tests/build/knip) yet were unusable in a real browser - a stray `//` in
// JSX rendered as a visible text node, and a dialog was stacked under a
// sibling. Neither defect is visible to a query-by-role/label RTL test, so
// this gate actually mounts every story in a real Chromium (Playwright, via
// Vitest browser mode) and inspects the rendered DOM instead.
const storyModules = import.meta.glob('./**/*.stories.tsx', { eager: true }) as Record<string, Record<string, unknown>>

const VIEWPORTS = [
  { name: 'mobile', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
] as const
const THEMES = ['light', 'dark'] as const

// Walks every text node in the subtree - a `TreeWalker` is the only DOM API
// that visits text nodes hidden inside deeply nested markup the way a stray
// JSX `// comment` would be.
// Skips text inside <pre>/<code> or a font-mono container - real tool output
// (grep matches, diffs, source snippets - ToolCallView renders these as plain
// font-mono <li>/<span>, not <pre>/<code>) legitimately contains lines
// starting with "//"; the bug class this guards is a STRAY comment-shaped
// text node loose in ordinary prose JSX children, never intentional
// code/data content.
function insideCodeBlock(node: Node): boolean {
  return !!(node.parentElement?.closest('pre, code, [class*="font-mono"]'))
}

function findStrayCommentText(root: Element): string | undefined {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  let node = walker.nextNode()
  while (node) {
    const text = node.textContent?.trim() ?? ''
    if ((text.startsWith('//') || text.startsWith('/*')) && !insideCodeBlock(node)) return text
    node = walker.nextNode()
  }
  return undefined
}

function findCoveredDialog(root: Element): string | undefined {
  const dialogs = root.querySelectorAll('[role="dialog"], [open]')
  for (const el of Array.from(dialogs)) {
    const rect = el.getBoundingClientRect()
    if (rect.width === 0 || rect.height === 0) continue
    const cx = rect.left + rect.width / 2
    const cy = rect.top + rect.height / 2
    const hit = document.elementFromPoint(cx, cy)
    if (!hit) continue
    if (hit !== el && !el.contains(hit)) {
      return `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ''} is covered by ${hit.tagName.toLowerCase()}`
    }
  }
  return undefined
}

describe.each(Object.entries(storyModules))('%s', (path, mod) => {
  const composed = composeStories(mod as never)

  describe.each(Object.entries(composed))('%s', (storyName, StoryComp) => {
    // Most existing stories render at a fixed desktop-ish width (e.g. a
    // `max-w-2xl` wrapper) with no mobile intent - the codebase's own
    // convention for a story that DOES care about a phone width is to say so
    // in its name (Composer's `MobileViewport360`, ArtifactPanel's
    // `WithJudgeNotesMobile`). Reuse that convention here rather than
    // fighting every desktop-only story's natural width at 390px.
    const viewports = /mobile/i.test(storyName) ? VIEWPORTS : VIEWPORTS.filter(v => v.name === 'desktop')
    describe.each(viewports)('$name viewport', ({ name: viewportName, width, height }) => {
      it.each(THEMES)('%s theme has no visible defects', async theme => {
        document.documentElement.classList.toggle('dark', theme === 'dark')
        await page.viewport(width, height)

        const errors: unknown[] = []
        const originalError = console.error
        console.error = (...args: unknown[]) => { errors.push(args); originalError(...args) }

        const Story = StoryComp as React.ComponentType
        // Most existing stories render a bare component with no app-shell
        // frame around it (that frame is normally what bounds width and
        // provides the "scroll inside your own container" boundary) - a raw
        // mount would make every such story "overflow" a 390px viewport
        // regardless of whether the component itself is at fault. Wrapping
        // every story in a fixed width x height clipped frame reproduces the
        // real app shell uniformly: a component with its own internal
        // overflow-x-auto (per the frontend-design skill's convention) stays
        // within this frame; one that pushes its own box wider does not.
        const { container } = render(
          <div style={{ width, height, overflow: 'hidden' }}>
            <Story />
          </div>,
        )
        // Effects that render async (MermaidDiagram's dynamic import + parse)
        // need a tick past the initial synchronous mount.
        await new Promise(r => setTimeout(r, 50))

        try {
          const strayComment = findStrayCommentText(container)
          expect(strayComment, `stray comment-like text node: ${strayComment}`).toBeUndefined()

          const coveredDialog = findCoveredDialog(container)
          expect(coveredDialog, coveredDialog).toBeUndefined()

          const frame = container.firstElementChild as HTMLElement
          expect(frame.scrollWidth).toBeLessThanOrEqual(frame.clientWidth)

          expect(errors, `console.error during render: ${JSON.stringify(errors)}`).toHaveLength(0)

          const safeName = path.replace(/[^a-zA-Z0-9]/g, '_')
          // `path` is relative to this test file and saved server-side by
          // Vitest's browser RPC - no fs access needed from browser code.
          await page.screenshot({ path: `../render-check/${safeName}__${storyName}__${viewportName}__${theme}.png` })
        } finally {
          console.error = originalError
          cleanup()
          document.documentElement.classList.remove('dark')
        }
      })
    })
  })
})
