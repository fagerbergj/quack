import { describe, it, expect } from 'vitest'
import { page } from 'vitest/browser'
import { render, cleanup } from '@testing-library/react'
import { composeStories, setProjectAnnotations } from '@storybook/react-vite'
import * as previewAnnotations from '../.storybook/preview'
import './index.css'

// #1192: two quack-authored PRs passed every gate (tsc/eslint/424 RTL
// tests/build/knip) yet were unusable in a real browser - a stray `//` in
// JSX rendered as a visible text node, and a dialog was stacked under a
// sibling. Neither defect is visible to a query-by-role/label RTL test, so
// this gate actually mounts every story in a real Chromium (Playwright, via
// Vitest browser mode) and inspects the rendered DOM instead. Importing
// index.css here (not just relying on the preview module doing it) makes the
// intent explicit even though `../.storybook/preview` already pulls it in -
// without SOME import of it, none of the Tailwind rules the checks below
// depend on (z-index stacking, overflow-x-auto, dark: variants) would exist
// in this document at all.
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

// Waits for the subtree to actually stop changing instead of a fixed sleep:
// web fonts loaded (layout-affecting), every <img> resolved, then a short
// idle window after the last DOM mutation. This is what a Mermaid story
// needs (its diagram arrives via a dynamic import + async render() call,
// swapping in an <svg> well after mount) without a Mermaid-specific branch -
// the mutation that adds the <svg> is exactly what resets the idle timer.
// Bounded overall so a story with a genuinely perpetual mutation (a live
// "running" spinner, say) can't hang the gate.
function waitForSettled(root: Element, { idleMs = 100, maxMs = 3000 } = {}): Promise<void> {
  return new Promise(resolve => {
    let done = false
    const finish = () => { if (!done) { done = true; observer.disconnect(); resolve() } }
    let idleTimer = setTimeout(finish, idleMs)
    const observer = new MutationObserver(() => {
      clearTimeout(idleTimer)
      idleTimer = setTimeout(finish, idleMs)
    })
    observer.observe(root, { childList: true, subtree: true, attributes: true, characterData: true })
    setTimeout(finish, maxMs)
  })
}

async function waitForRenderSettled(root: Element): Promise<void> {
  await document.fonts.ready
  const images = Array.from(root.querySelectorAll('img'))
  await Promise.all(images.map(img => (img.complete ? Promise.resolve() : new Promise<void>(res => {
    img.addEventListener('load', () => res(), { once: true })
    img.addEventListener('error', () => res(), { once: true })
  }))))
  await waitForSettled(root)
}

describe.each(Object.entries(storyModules))('%s', (path, mod) => {
  // Composed once here just to enumerate story names - the per-theme render
  // below recomposes with that theme's project annotations so the preview's
  // real `withTheme` decorator (not a hand-rolled duplicate of it) drives the
  // `dark` class, the same mechanism MermaidDiagram's own useIsDarkMode and
  // the shipped app both rely on.
  const storyNames = Object.keys(composeStories(mod as never))

  describe.each(storyNames)('%s', storyName => {
    it.each(THEMES)('%s theme has no visible defects at every opted-in viewport', async theme => {
      setProjectAnnotations({ ...(previewAnnotations as unknown as Record<string, unknown>), globals: { theme } } as never)
      const composed = composeStories(mod as never)
      const StoryComp = composed[storyName] as React.ComponentType & { parameters?: Record<string, unknown> }

      // Viewport opt-in: a story's own `parameters.renderCheck.viewports` (or
      // Storybook's own `parameters.viewport.defaultViewport`, honored as an
      // equivalent signal) wins when present. Name-matching ("...Mobile...",
      // matching the codebase's own MobileViewport360/WithJudgeNotesMobile
      // convention) is only the FALLBACK for stories that haven't opted in
      // explicitly yet - most existing stories render at a fixed desktop
      // width with no mobile intent, so checking them at 390px would flag
      // the story's own width choice, not a component defect.
      const renderCheckParams = StoryComp.parameters?.renderCheck as { viewports?: readonly string[] } | undefined
      const storybookViewport = StoryComp.parameters?.viewport as { defaultViewport?: string } | undefined
      const wantsMobile = renderCheckParams?.viewports
        ? renderCheckParams.viewports.includes('mobile')
        : storybookViewport?.defaultViewport
          ? storybookViewport.defaultViewport.toLowerCase().includes('mobile')
          : /mobile/i.test(storyName)
      const viewports = wantsMobile ? VIEWPORTS : VIEWPORTS.filter(v => v.name === 'desktop')

      for (const { name: viewportName, width, height } of viewports) {
        await page.viewport(width, height)

        const errors: unknown[] = []
        const originalError = console.error
        console.error = (...args: unknown[]) => { errors.push(args); originalError(...args) }

        try {
          // Most existing stories render a bare component with no app-shell
          // frame around it (that frame is normally what bounds width and
          // provides the "scroll inside your own container" boundary) - a
          // raw mount would make every such story "overflow" a 390px
          // viewport regardless of whether the component itself is at
          // fault. Wrapping every story in a fixed width x height clipped
          // frame reproduces the real app shell uniformly: a component with
          // its own internal overflow-x-auto (per the frontend-design
          // skill's convention) stays within this frame; one that pushes
          // its own box wider does not.
          const { container } = render(
            <div style={{ width, height, overflow: 'hidden' }}>
              <StoryComp />
            </div>,
          )
          await waitForRenderSettled(container)

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
        }
      }
    })
  })
})
