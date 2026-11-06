export type Finding = { severity: string; kind?: string; loc?: string; title: string; more: string };
export type Review = { takeaway: string; verdict: string; findings: Finding[]; verified: string[]; notes: string[] };

const sections = (md: string) => new Map(md.split(/^## /m).slice(1).map((s) => [s.slice(0, s.indexOf('\n')).trim().toLowerCase(), s.slice(s.indexOf('\n') + 1).trim()]));

// Top-level "- " lines start an item; a "- " inside a code fence belongs to the item above.
function items(text: string): string[] {
  const out: string[] = [];
  let fenced = false;
  for (const line of text.split('\n')) {
    if (line.startsWith('```')) fenced = !fenced;
    if (!fenced && line.startsWith('- ')) out.push(line.slice(2));
    else if (out.length) out[out.length - 1] += `\n${line}`;
  }
  return out.map((i) => i.trim());
}

const HEAD = /^([a-z]+)(?: \(([^)]*)\))?: (?:(\S+?:\d+(?:-\d+)?) — )?([\s\S]*)$/i;
const SENTENCE = /^([\s\S]{16,}?(?<!\b(?:e\.g|i\.e))[.!?])\s+(?=[A-Z`])/;
const TITLE_MAX = 140;
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function cutTitle(s: string): string {
  const t = s.length > TITLE_MAX ? s.slice(0, TITLE_MAX).replace(/\s+\S*$/, '') : s;
  return t.length < s.length ? `${t}…` : t;
}

function finding(item: string): Finding {
  const m = HEAD.exec(item);
  if (!m) return { severity: 'note', title: cutTitle(item.split('\n')[0]!), more: item };
  const [, severity, kind, loc, rest] = m;
  const [first, ...tail] = rest!.split('\n');
  const head = SENTENCE.exec(first!);
  const title = cap(cutTitle(head ? head[1]! : first!));
  // The first paragraph's remainder and everything after it (a code block, say) is the "why and fix".
  const lead = title.endsWith('…') ? first! : head ? first!.slice(head[0].length) : '';
  const more = [lead, ...tail].join('\n').trim();
  return { severity: severity!.toLowerCase(), kind, loc, title, more };
}

/** A review rendered from its markdown (takeaway, verdict, findings, verified, notes); undefined when the text is not one. */
export function parseReview(md: string): Review | undefined {
  const s = sections(md);
  const verdict = s.get('verdict')?.split(/\s/)[0];
  if (!verdict) return undefined;
  return {
    takeaway: s.get('takeaway') ?? '',
    verdict,
    findings: items(s.get('findings') ?? '').map(finding),
    verified: items(s.get('verified') ?? ''),
    notes: items(s.get('notes') ?? ''),
  };
}
