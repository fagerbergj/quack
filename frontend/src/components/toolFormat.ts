// Small formatting helpers for rendering tool calls.

// summarizeArgs picks a representative arg (a query/id) to show beside the tool
// name in the collapsed summary, so a call is identifiable without expanding it.
export function summarizeArgs(args: Record<string, unknown>): string {
  for (const key of ['query', 'url', 'id', 'q']) {
    const v = args[key]
    if (typeof v === 'string' && v) return JSON.stringify(v)
  }
  return ''
}

// prettyJSON renders a value as indented JSON, falling back to String on cycles.
export function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}
