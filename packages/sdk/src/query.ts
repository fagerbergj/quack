// Lucene term escaping for the catalog's q, shared by everything that builds a query in code and by the parser.

const KEYWORDS = new Set(['AND', 'OR', 'NOT']);

/** A value written into q as one literal term: every character but a letter, digit or underscore is backslash-escaped. */
export function escapeTerm(value: string): string {
  const escaped = value.replace(/[^\p{L}\p{N}_]/gu, '\\$&');
  // A bare AND, OR or NOT would read as an operator; an escaped letter is still that letter.
  return KEYWORDS.has(escaped) ? `\\${escaped}` : escaped;
}

/** An escaped term's literal text, and the indexes in it of each unescaped * and ? (the wildcards). */
export function unescapeTerm(raw: string): { text: string; wildcards: number[] } {
  let text = '';
  const wildcards: number[] = [];
  for (let i = 0; i < raw.length; i++) {
    const c = raw[i]!;
    if (c === '\\' && i + 1 < raw.length) text += raw[++i];
    else {
      if (c === '*' || c === '?') wildcards.push(text.length);
      text += c;
    }
  }
  return { text, wildcards };
}
