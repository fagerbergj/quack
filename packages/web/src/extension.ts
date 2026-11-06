import { createContext, type ComponentType, type ReactNode } from 'react';
import type { Approval, ArtifactLink, SealedArtifact } from './api.ts';
import type { Row } from './sessions.ts';

/** The session an artifact is shown from, for links that open it on its own page. */
export const SessionKeyContext = createContext<string | undefined>(undefined);

export type Tone = 'plain' | 'muted' | 'warn' | 'ok' | 'bad';
export type Words = { run: string; pass: string; fail: string; none: string; /** Newest artifact sealed without a verdict (kind is never judged). */ sealed?: string };
export type Delivered = { line: string; tone: Tone; link?: ArtifactLink };
export type Named = { title: string; full: string; num?: string; repo?: string };

/** How one family of sessions reads in lists and headers; the first kind whose `match` hits names the row. */
export type SessionKind = {
  label: string;
  /** What this kind's artifacts are called on the Artifacts page ("Review"), and the filter chip that groups them ("Reviews"). */
  noun?: (r: Row) => string;
  group?: string;
  match: (r: Row) => boolean;
  icon: string;
  tint: string;
  name: (r: Row) => Named;
  words: (r: Row) => { kind: string; w: Words } | undefined;
  /** What an idle session delivered, with its outside link; undefined leaves the line to `words`. A running session always reads `words.run`. */
  delivered?: (r: Row) => Delivered | undefined;
};

/** What a studio pane is given: the session's live artifacts and pending approvals, whether a run works, and `decided` to drop an approval it settled. */
export type StudioProps = { sessionKey: string; artifacts: SealedArtifact[]; approvals: Approval[]; running: boolean; decided: (id: string) => void };

/** What an extension's web module hands the shell; every field is optional. */
export type Extension = {
  session?: SessionKind;
  /** Session-key prefix and the label the "started by" filter shows for it. */
  start?: { prefix: string; label: string };
  /** What one of this extension's artifacts is called on the transcript line ("Review"), with a posted state when `posted`; undefined when it is not its artifact. */
  artifact?: (a: SealedArtifact, running: boolean) => { name: string; posted?: string; /** Posted for an earlier request: no verdict icon while a new one runs. */ previous?: boolean; /** Its card draws a passed check beside its own title, so the line carries none. */ titled?: boolean; /** How the context rail says a posted version went out ("Posted to GitHub as Approve"); "Posted" when absent. */ postedTo?: string } | undefined;
  /** A sealed artifact drawn as a card instead of a line; undefined when it is not this extension's or cannot be read as one. */
  card?: (a: SealedArtifact, now?: { running: boolean; meta: readonly Record<string, unknown>[] }) => ReactNode | undefined;
  /** The artifact drawn as its own page (opened from a link on its card); undefined when it is not this extension's. */
  full?: (a: SealedArtifact) => ReactNode | undefined;
  /** The version a kind's content calls itself, when sealed versions repeat it (a revised draft); undefined otherwise. */
  versionOf?: (a: SealedArtifact) => number | undefined;
  /** Plain words for a tool this extension owns while it runs, by tool name, given the call's args. */
  steps?: Record<string, (args: Record<string, unknown>) => string>;
  /** A user message drawn as a card (a review's opening), given the session's meta records; undefined for any other text. */
  opening?: (text: string, meta: readonly Record<string, unknown>[]) => ReactNode | undefined;
  /** A starter the home screen offers: the pill shows `label`, a click fills the composer with `prompt`. */
  suggestion?: { label: string; prompt: string };
  /** A badge beside a session's name in the list and its header (a pull request's state); it draws nothing for a session that is not its own. */
  chip?: ComponentType<{ sessionKey: string }>;
  /** Wraps the shell to share what its chips read across the listed sessions, such as one lookup for every row. */
  provider?: ComponentType<{ rows: Row[] | undefined; children: ReactNode }>;
  /** A line above the session list when the extension has something to say about it (its lookups failed). */
  notice?: ComponentType;
  /** A live pane beside the transcript of the sessions it matches (a phone tabs between them, named `label`). The approvals it `owns` are decided
   * there, not as transcript cards, and a message sent while one waits is steered into the run and declines it, so the agent reads it at once. */
  studio?: { match: (key: string) => boolean; label: string; owns: (a: Approval) => boolean; Pane: ComponentType<StudioProps> };
  /** A section at the top of a session's context rail; it draws nothing for a session that is not its own. */
  rail?: ComponentType<{ sessionKey: string; title: string; opening?: string; meta: readonly Record<string, unknown>[] }>;
  /** An outside page for a session, shown as a header link. */
  link?: (key: string) => { url: string; label: string } | undefined;
  /** Judge unit ids that stand for the whole artifact; a note prefixed with one needs no chip (verdicts sealed before `scope` was recorded). */
  wholeUnits?: string[];
  /** Groups sessions (a repo, say); the group shows in titles only when sessions span more than one. */
  scope?: (key: string) => string | undefined;
  /** A nav entry under Apps; without `page` it shows the placeholder. */
  nav?: {
    route: string; label: string; icon: string; tint?: string; page?: ComponentType<{ rows: Row[] | undefined }>;
    /** In the bottom bar until the person picks their own pages. */
    pinned?: boolean;
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
