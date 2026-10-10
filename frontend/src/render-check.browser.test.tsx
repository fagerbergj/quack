import { describe, it, expect } from 'vitest'
import { page } from 'vitest/browser'
import { render, cleanup } from '@testing-library/react'
import { composeStories, setProjectAnnotations } from '@storybook/react-vite'
import * as previewAnnotations from '../.storybook/preview'
import './index.css'

// RTL can't see browser-only defects (a stray `//` JSX text node, a mis-stacked dialog), so this mounts every story
// in real Chromium. index.css is imported so the Tailwind rules the checks need exist in this document.
const storyModules = import.meta.glob('./**/*.stories.tsx', { eager: true }) as Record<string, Record<string, unknown>>

const VIEWPORTS = [
  { name: 'mobile', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
] as const
const THEMES = ['light', 'dark'] as const

// TreeWalker is the only DOM API that reaches deeply nested text nodes, where a stray JSX `//` lands.
// Skips <pre>/<code>/font-mono: real tool output can start lines with "//".
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
  const dialogs = root.querySelectorAll('[role="dialog"], dialog[open]')
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

// Waits for the subtree to settle (fonts, every <img>, then an idle window after the last mutation), which covers
// Mermaid's async <svg> without a special case. Bounded so a perpetual mutation (a spinner) can't hang the gate.
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
  // Composed once only to enumerate story names; the per-theme render recomposes with that theme's annotations so
  // the preview's real `withTheme` decorator drives the `dark` class, as in the shipped app.
  const storyNames = Object.keys(composeStories(mod as never))

  describe.each(storyNames)('%s', storyName => {
    it.each(THEMES)('%s theme has no visible defects at every opted-in viewport', async theme => {
      // Portable stories read project globals from `initialGlobals`; plain
      // `globals` never reaches the withTheme decorator, so dark == light.
      setProjectAnnotations({ ...(previewAnnotations as unknown as Record<string, unknown>), initialGlobals: { theme } } as never)
      const composed = composeStories(mod as never)
      const StoryComp = composed[storyName] as React.ComponentType & { parameters?: Record<string, unknown>; play?: (ctx: { canvasElement: HTMLElement }) => Promise<void> }
      // Pages that mount useTheme re-resolve the theme from storage on mount; without this they'd override the
      // decorator with "system" (headless Chromium's light preference).
      localStorage.setItem('theme', theme)

      // A story's `parameters.renderCheck.viewports` (or `parameters.viewport.defaultViewport`) wins; name-matching
      // ("...Mobile...") is only the fallback, since most stories render at a fixed desktop width.
      const renderCheckParams = StoryComp.parameters?.renderCheck as { viewports?: readonly string[]; play?: boolean; strictPlay?: boolean } | undefined
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
          // Bare stories lack the app shell that bounds width, so each mounts in a fixed clipped frame: a component
          // with its own overflow-x-auto stays inside, one that pushes its box wider overflows.
          const { container } = render(
            <div style={{ width, height, overflow: 'hidden' }}>
              <StoryComp />
            </div>,
          )
          await waitForRenderSettled(container)
          // ponytail: an opt-in play() (opens menus/sheets) only warns on failure unless the story sets strictPlay;
          // four story files stomp window.fetch at module scope, so fetch-driven plays can't be made reliable here.
          if (renderCheckParams?.play && StoryComp.play) {
            try {
              await StoryComp.play({ canvasElement: container })
            } catch (e) {
              if (renderCheckParams.strictPlay) throw e
              console.warn(`play() failed for ${path} ${storyName}: ${String(e).split('\n')[0]}`)
            }
            await waitForRenderSettled(container)
          }

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
          // Mermaid appends `d<id>` to <body>, outside RTL's cleanup, and a parse error leaves its error SVG there,
          // stacking up in every later screenshot.
          document.querySelectorAll('body > [id^="dmermaid-"], body > [id^="mermaid-"]').forEach(el => el.remove())
        }
      }
    })
  })
})
