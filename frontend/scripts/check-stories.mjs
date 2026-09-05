#!/usr/bin/env node
// Fails CI when a component .tsx file under src/components or src/pages has
// no matching *.stories.tsx sibling (issue #1192: quack-authored PRs shipped
// visible defects that no story ever exercised).
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')
const dirs = ['components', 'pages']

// pages/Chat.tsx: allowlisted for #1217 - remove this entry once
// pages/Chat.stories.tsx exists.
const ALLOWLIST = new Set(['pages/Chat.tsx'])

// A .tsx file counts as a component file if it exports a capitalized
// function, const/arrow, or memo() component - excludes plain utility
// modules (envelope.ts-style logic that happens to live in a .tsx file, if
// any). Must also match `export const X = memo(...)`/arrow forms (DagNode,
// TurnView, MermaidDiagram) - a function-declaration-only regex would let a
// future arrow component with no story pass silently, the exact false
// negative #1192 created this gate to catch.
const COMPONENT_EXPORT = /export\s+(?:default\s+function\s+[A-Z]\w*\s*\(|function\s+[A-Z]\w*\s*\(|const\s+[A-Z]\w*\s*(?::[^=]+)?=\s*(?:memo\(|\())/

function walk(dir) {
  const out = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else out.push(full)
  }
  return out
}

const missing = []
for (const dir of dirs) {
  for (const file of walk(join(root, dir))) {
    if (!file.endsWith('.tsx')) continue
    if (file.endsWith('.stories.tsx') || file.endsWith('.test.tsx')) continue
    const rel = file.slice(root.length + 1)
    if (ALLOWLIST.has(rel)) continue
    const content = readFileSync(file, 'utf8')
    if (!COMPONENT_EXPORT.test(content)) continue
    const storyPath = file.replace(/\.tsx$/, '.stories.tsx')
    try {
      statSync(storyPath)
    } catch {
      missing.push(rel)
    }
  }
}

if (missing.length > 0) {
  console.error('Missing *.stories.tsx for:')
  for (const m of missing) console.error(`  - ${m}`)
  process.exit(1)
}
console.log('check-stories: every component has a story.')
