#!/usr/bin/env node
// Changed-line coverage gate for the frontend: lines added in
// <base>...HEAD (frontend/src, non-test, non-generated) must sit at >=70%
// covered statements per the lcov report from the coverage run. Lines without
// a statement (imports, types, JSX-only lines) stay out of the denominator,
// so a test-only PR passes trivially and legacy gaps never block.
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import process from 'node:process'

const MIN_PCT = 70
const base = process.argv[2]
if (!base) {
  console.error('usage: coverage-diff.mjs <git-base-ref>')
  process.exit(2)
}

// --- parse lcov.info: file -> Map(line -> hits) ---
const lcov = parseLcov(readFileSync(new URL('../frontend/coverage/lcov.info', import.meta.url), 'utf8'))

// --- changed lines from the diff (new-file numbering) ---
const diff = execFileSync('git', ['diff', '-U0', `${base}...HEAD`, '--', 'frontend/src'], { encoding: 'utf8' })
const hunk = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@/
const changed = new Map() // repo-relative path -> Set(line)
let cur = null
for (const line of diff.split('\n')) {
  if (line.startsWith('+++ b/')) {
    const p = line.slice(6)
    cur = isGated(p) ? p : null
    if (cur) changed.set(cur, new Set())
    continue
  }
  const m = line.match(hunk)
  if (m && cur) {
    const start = +m[1]
    const n = m[2] === undefined ? 1 : +m[2]
    for (let i = 0; i < n; i++) changed.get(cur).add(start + i)
  }
}

function isGated(p) {
  return (
    p.startsWith('frontend/src/') &&
    !p.startsWith('frontend/src/generated/') &&
    !/(\.|\/)(test|spec|story)\.(ts|tsx)$/.test(p) &&
    /\.(ts|tsx)$/.test(p)
  )
}

function parseLcov(text) {
  const files = new Map()
  let cur = null
  for (const line of text.split('\n')) {
    if (line.startsWith('SF:')) {
      cur = line.slice(3)
      if (!files.has(cur)) files.set(cur, new Map())
    } else if (line.startsWith('DA:') && cur) {
      const [ln, hits] = line.slice(3).split(',').map(Number)
      files.get(cur).set(ln, hits)
    } else if (line === 'end_of_record') {
      cur = null
    }
  }
  return files
}

// --- match ---
let num = 0
let den = 0
const misses = []
for (const [p, lines] of changed) {
  // lcov SF paths are absolute on the runner; match by suffix.
  const cov = [...lcov.entries()].find(([f]) => f.endsWith('/' + p) || f.endsWith(p))?.[1]
  if (!cov) continue // file never loaded (e.g. no test imports it) - see note below
  for (const ln of lines) {
    if (!cov.has(ln)) continue // no statement on this line
    den++
    if (cov.get(ln) > 0) num++
    else misses.push(`${p}:${ln}`)
  }
}

if (den === 0) {
  console.log('coverage-diff: no changed gated lines carry statements')
  process.exit(0)
}
const pct = (num * 100) / den
if (pct < MIN_PCT) {
  console.error(`coverage-diff: changed-line coverage ${pct.toFixed(1)}% < ${MIN_PCT}% (${num}/${den} statements)`)
  for (const m of misses.slice(0, 30)) console.error(`  uncovered: ${m}`)
  if (misses.length > 30) console.error(`  …and ${misses.length - 30} more`)
  process.exit(1)
}
console.log(`coverage-diff: ok - changed-line coverage ${pct.toFixed(1)}% (${num}/${den} statements)`)
