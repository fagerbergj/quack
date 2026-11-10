import type { ArtifactKind } from '@quack/sdk';

export type Transcript = {
  kind: 'remarkable.transcript';
  document: { id: string; name: string; version: string };
  topic: string;
  pages: Array<{ page: number; text: string }>;
};

/** One non-empty text section per rendered page, numbered from 1 with no gaps (the audit's completeness check). */
export function problems(t: Partial<Transcript> | null): string[] {
  if (t?.kind !== 'remarkable.transcript') return ['kind must be "remarkable.transcript"'];
  const out: string[] = [];
  if (!t.document?.id || !t.document.name || !t.document.version) out.push('document needs an id, name and version');
  if (!t.topic?.trim()) out.push('topic is empty');
  if (!t.pages?.length) out.push('there are no pages');
  (t.pages ?? []).forEach((p, i) => {
    if (p.page !== i + 1) out.push(`page ${i + 1} is numbered ${p.page}`);
    if (!p.text?.trim()) out.push(`page ${i + 1} has no text`);
  });
  return out;
}

export const transcriptKind: ArtifactKind = {
  kind: 'remarkable.transcript',
  path: 'transcript.json',
  summarize(text) {
    const t = JSON.parse(text) as Partial<Transcript>;
    const n = t.pages?.length ?? 0;
    return `${t.topic ?? t.document?.name ?? ''} · ${n} ${n === 1 ? 'page' : 'pages'}`;
  },
  validate(text) {
    try {
      return problems(JSON.parse(text));
    } catch {
      return ['transcript.json is not JSON'];
    }
  },
};
