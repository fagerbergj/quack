// Validates one mermaid diagram (read from stdin) against the REAL
// mermaid.parse() - not a reimplementation, see internal/vetting/mermaid.go
// and #574 - headless under a jsdom shim. Prints {"ok":bool,"error":string}.
import { JSDOM } from 'jsdom'

const dom = new JSDOM('<!DOCTYPE html><body></body>', { pretendToBeVisual: true })
globalThis.window = dom.window
globalThis.document = dom.window.document
// globalThis.navigator = … throws on modern Node (getter-only property).
Object.defineProperty(globalThis, 'navigator', { value: dom.window.navigator, configurable: true })

// GitHub's mermaid.js version as of #574 (probed via the `info` diagram) -
// re-check on upgrade, GitHub pins its own and the two can drift.
const mermaid = (await import('mermaid')).default
mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' })

const chunks = []
for await (const chunk of process.stdin) chunks.push(chunk)

try {
  await mermaid.parse(Buffer.concat(chunks).toString('utf8'))
  console.log(JSON.stringify({ ok: true }))
} catch (err) {
  console.log(JSON.stringify({ ok: false, error: String(err?.message ?? err) }))
}
