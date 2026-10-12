// Builds Storybook and loads every story at 390/1280 x light/dark in Playwright, failing on a console
// error or horizontal overflow. Standalone because this package has no test runner.
import { execFileSync } from 'node:child_process'
import { createServer } from 'node:http'
import { readFile, stat } from 'node:fs/promises'
import path from 'node:path'
import { chromium } from 'playwright'

const ROOT = path.join(import.meta.dirname, 'storybook-static')
const VIEWPORTS = [
  { name: '390', width: 390, height: 844 },
  { name: '1280', width: 1280, height: 900 },
]
const THEMES = ['light', 'dark']

function buildStorybook() {
  execFileSync('npx', ['storybook', 'build', '-o', 'storybook-static', '--quiet'], { cwd: import.meta.dirname, stdio: 'inherit' })
}

async function serveStatic() {
  const server = createServer(async (req, res) => {
    // Malformed percent-encoding (e.g. "%%zz") throws URIError; caught here
    // so it 404s as a literal path instead of crashing the async handler.
    let p
    try { p = decodeURIComponent(req.url.split('?')[0]) } catch { p = req.url.split('?')[0] }
    if (p === '/') p = '/index.html'
    // Reject a resolved path that escapes ROOT (e.g. an encoded ../) before
    // touching the filesystem - CI-only and single-client, but cheap to close.
    const full = path.resolve(path.join(ROOT, p))
    if (full !== ROOT && !full.startsWith(ROOT + path.sep)) {
      res.statusCode = 404
      res.end('not found')
      return
    }
    try {
      const s = await stat(full)
      if (s.isFile()) {
        res.setHeader('Content-Type', contentType(full))
        res.end(await readFile(full))
        return
      }
    } catch { /* fall through to 404 */ }
    res.statusCode = 404
    res.end('not found')
  })
  // Loopback only - this server has no reason to accept non-local connections.
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  return server
}

function contentType(file) {
  const ext = path.extname(file)
  return { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2' }[ext] || 'application/octet-stream'
}

// Pinned so a renamed story file, a deleted variant export, or a broken
// glob (which would otherwise just shrink the loop silently) fails loudly.
const EXPECTED_STORIES = 61

async function storyIDs() {
  const index = JSON.parse(await readFile(path.join(ROOT, 'index.json'), 'utf8'))
  const ids = Object.values(index.entries).filter(e => e.type === 'story').map(e => e.id)
  if (ids.length !== EXPECTED_STORIES) {
    throw new Error(`found ${ids.length} stories, expected ${EXPECTED_STORIES} - update EXPECTED_STORIES if this is a deliberate story change`)
  }
  return ids
}

async function checkStory(page, base, id, viewport, theme) {
  await page.setViewportSize({ width: viewport.width, height: viewport.height })
  const errors = []
  const onConsole = msg => { if (msg.type() === 'error') errors.push(msg.text()) }
  page.on('console', onConsole)
  page.on('pageerror', err => errors.push(String(err)))
  const url = `${base}/iframe.html?id=${id}&viewMode=story&globals=theme:${theme}`
  await page.goto(url, { waitUntil: 'networkidle' })
  await page.waitForTimeout(150) // fonts/late layout settle, same margin as quack's own render-check
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  page.off('console', onConsole)
  const failures = []
  if (errors.length) failures.push(`console errors: ${errors.join(' | ')}`)
  if (overflow > 1) failures.push(`horizontal overflow: ${overflow}px wider than the viewport`)
  return failures
}

async function main() {
  buildStorybook()
  const server = await serveStatic()
  const base = `http://127.0.0.1:${server.address().port}`
  const browser = await chromium.launch()
  const ids = await storyIDs()

  let failed = 0
  const page = await browser.newPage()
  for (const id of ids) {
    for (const viewport of VIEWPORTS) {
      for (const theme of THEMES) {
        const failures = await checkStory(page, base, id, viewport, theme)
        if (failures.length) {
          failed++
          console.error(`FAIL ${id} @ ${viewport.name} ${theme}: ${failures.join('; ')}`)
        }
      }
    }
  }
  await browser.close()
  server.close()

  const total = ids.length * VIEWPORTS.length * THEMES.length
  console.log(`render-check: ${total - failed}/${total} passed`)
  if (failed > 0) process.exit(1)
}

main().catch(err => { console.error(err); process.exit(1) })
