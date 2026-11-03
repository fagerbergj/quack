import type { ComponentType, ReactNode } from 'react';
import type { ArtifactLink, SealedArtifact } from './api.ts';
import type { Row } from './sessions.ts';

export type Tone = 'plain' | 'muted' | 'warn' | 'ok' | 'bad';
export type Words = { run: string; pass: string; fail: string; none: string; /** Newest artifact sealed without a verdict (kind is never judged). */ sealed?: string };
export type Delivered = { line: string; tone: Tone; link?: ArtifactLink };
export type Named = { title: string; full: string; num?: string; repo?: string };

/** How one family of sessions reads in lists and headers; the first kind whose `match` hits names the row. */
export type SessionKind = {
  label: string;
  match: (r: Row) => boolean;
  icon: string;
  tint: string;
  name: (r: Row) => Named;
  words: (r: Row) => { kind: string; w: Words } | undefined;
  /** What an idle session delivered, with its outside link; undefined leaves the line to `words`. A running session always reads `words.run`. */
  delivered?: (r: Row) => Delivered | undefined;
};

/** What an extension's web module hands the shell; every field is optional. */
export type Extension = {
  session?: SessionKind;
  /** Session-key prefix and the label the "started by" filter shows for it. */
  start?: { prefix: string; label: string };
  /** What one of this extension's artifacts is called on the transcript line ("Review"), with a posted state when `posted`; undefined when it is not its artifact. */
  artifact?: (a: SealedArtifact, running: boolean) => { name: string; posted?: string; /** Posted for an earlier request: no verdict icon while a new one runs. */ previous?: boolean } | undefined;
  /** A sealed artifact drawn as a card instead of a line; undefined when it is not this extension's or cannot be read as one. */
  card?: (a: SealedArtifact) => ReactNode | undefined;
  /** A user message drawn as a card (a review's opening), given the session's meta records; undefined for any other text. */
  opening?: (text: string, meta: readonly Record<string, unknown>[]) => ReactNode | undefined;
  /** A starter the home screen offers: the pill shows `label`, a click fills the composer with `prompt`. */
  suggestion?: { label: string; prompt: string };
  /** An outside page for a session, shown as a header link. */
  link?: (key: string) => { url: string; label: string } | undefined;
  /** Judge unit ids that stand for the whole artifact; a note prefixed with one needs no chip (verdicts sealed before `scope` was recorded). */
  wholeUnits?: string[];
  /** Groups sessions (a repo, say); the group shows in titles only when sessions span more than one. */
  scope?: (key: string) => string | undefined;
  /** A nav entry under Apps; without `page` it shows the placeholder. */
  nav?: {
    route: string; label: string; icon: string; tint?: string; page?: ComponentType<{ rows: Row[] | undefined }>;
    /** A hook for the plain line under its tile on the More page; `warn` marks something waiting. */
    status?: () => { text: string; warn?: boolean } | undefined;
  };
};

export const BLUE = 'bg-link-bg text-link';
const MAX = 40;

// Cuts at a word boundary within MAX characters; short text passes through.
export function cut(s: string): string {
  const t = s.replace(/\s+/g, ' ').trim();
  if (t.length <= MAX) return t;
  const head = t.slice(0, MAX);
  const at = head.lastIndexOf(' ');
  return `${(at > MAX / 2 ? head.slice(0, at) : head).replace(/[\s,.;:(-]+$/, '')}…`;
}

export const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
export const plain = (full: string, text = cut(full)): Named => ({ title: text, full });
