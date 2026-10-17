import { describe, it, expect } from 'vitest'

const sources = import.meta.glob<string>('../**/*.tsx', { query: '?raw', import: 'default', eager: true })

// A `${CHIP}` inside className="..." ships as literal text, and the badge silently loses its shape (#1794).
describe('className literals', () => {
  it('never hold a template placeholder', () => {
    const bad = Object.entries(sources).flatMap(([file, text]) =>
      text.split('\n').flatMap((line, i) => (/className="[^"]*\$\{/.test(line) ? [`${file}:${i + 1}`] : [])))
    expect(bad).toEqual([])
  })
})
