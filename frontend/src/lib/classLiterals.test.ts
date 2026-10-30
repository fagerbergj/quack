import { describe, it, expect } from 'vitest'

const sources = import.meta.glob<string>('../**/*.tsx', { query: '?raw', import: 'default', eager: true })

// A `${...}` inside className="..." ships as literal text, and the element silently loses its styles (#1794).
describe('className literals', () => {
  it('never hold a template placeholder', () => {
    const bad = Object.entries(sources).flatMap(([file, text]) =>
      text.split('\n').flatMap((line, i) => (/className="[^"]*\$\{/.test(line) ? [`${file}:${i + 1}`] : [])))
    expect(bad).toEqual([])
  })
})
