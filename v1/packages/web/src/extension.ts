import type { ComponentType } from 'react';
import type { Row } from './sessions.ts';

export type Tone = 'plain' | 'muted' | 'warn' | 'ok' | 'bad';
export type Words = { run: string; pass: string; fail: string; none: string };
export type Named = { title: string; full: string; num?: string; repo?: string };

/** How one family of sessions reads in lists and headers; the first kind whose `match` hits names the row. */
export type SessionKind = {
  label: string;
  match: (r: Row) => boolean;
  icon: string;
  tint: string;
  name: (r: Row) => Named;
  words: (r: Row) => { kind: string; w: Words } | undefined;
};

/** What an extension's web module hands the shell; every field is optional. */
export type Extension = {
  session?: SessionKind;
  /** Session-key prefix and the label the "started by" filter shows for it. */
  start?: { prefix: string; label: string };
  /** An outside page for a session, shown as a header link. */
  link?: (key: string) => { url: string; label: string } | undefined;
  /** Groups sessions (a repo, say); the group shows in titles only when sessions span more than one. */
  scope?: (key: string) => string | undefined;
  /** A nav entry under Apps; without `page` it shows the placeholder. */
  nav?: { route: string; label: string; icon: string; page?: ComponentType<{ rows: Row[] | undefined }> };
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
