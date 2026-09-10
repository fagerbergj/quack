#!/usr/bin/env node
// Fails CI when an emoji lands in src/*.ts(x) as a control/badge/label glyph
// (the sweep that replaced them with Material icons, see PR body). An
// allowlist covers the few kept for now pending an owner decision, or dense
// meaning with no Material equivalent (each entry must say why in the PR).
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, dirname, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')

// Emoji ranges only - excludes plain typography (arrows, box-drawing,
// ellipsis) that check-no-emoji isn't concerned with.
const EMOJI = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}]/gu

// An intentional emoji is allowed by a pragma ON THE SAME LINE, with a reason:
//   `// allow-emoji: <why>` - a file:line allowlist silently drifted on every
// edit above it (an entry stopped pointing at any emoji, or masked a new one).
const ALLOW_PRAGMA = /allow-emoji:\s*\S/

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    const st = statSync(p)
    if (st.isDirectory()) { walk(p, out); continue }
    if (!/\.(ts|tsx)$/.test(name)) continue
    if (name.endsWith('.test.ts') || name.endsWith('.test.tsx') || name.endsWith('.stories.tsx')) continue
    if (p.includes(`${join('src', 'generated')}${'/'}`)) continue
    out.push(p)
  }
  return out
}

let failed = false
for (const file of walk(root)) {
  const rel = relative(root, file)
  const lines = readFileSync(file, 'utf8').split('\n')
  lines.forEach((line, i) => {
    if (!EMOJI.test(line)) return
    EMOJI.lastIndex = 0
    if (ALLOW_PRAGMA.test(line)) return
    console.error(`${rel}:${i + 1}: emoji found - use the Icon component (frontend/src/components/Icon.tsx) or append "// allow-emoji: <reason>" to this line`)
    failed = true
  })
}

if (failed) process.exit(1)
console.log('check-no-emoji: OK')
