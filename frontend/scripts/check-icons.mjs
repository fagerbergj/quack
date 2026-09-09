#!/usr/bin/env node
// Fails CI when Icon.tsx's PATHS drift from icons.lock.json, the last-known-
// official Material Symbols snapshot (see the source note atop Icon.tsx).
// Regenerate the lock file deliberately when a path is intentionally updated.
import { readFileSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'components')
const src = readFileSync(join(root, 'Icon.tsx'), 'utf8')
const lock = JSON.parse(readFileSync(join(root, 'icons.lock.json'), 'utf8'))

const body = src.match(/const PATHS = \{([\s\S]*?)\n\} as const/)[1]
const current = Object.fromEntries(
  [...body.matchAll(/(\w+): '([^']*)',/g)].map(m => [m[1], m[2]]),
)

let failed = false
const names = new Set([...Object.keys(current), ...Object.keys(lock)])
for (const name of names) {
  if (!(name in lock)) {
    console.error(`${name}: new icon has no entry in icons.lock.json - add one from the official SVG`)
    failed = true
  } else if (!(name in current)) {
    console.error(`${name}: in icons.lock.json but missing from Icon.tsx PATHS`)
    failed = true
  } else if (current[name] !== lock[name]) {
    console.error(`${name}: PATHS value doesn't match icons.lock.json - copy from the official SVG, don't hand-edit`)
    failed = true
  }
}

if (failed) process.exit(1)
console.log('check-icons: OK')
