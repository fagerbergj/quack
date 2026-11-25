// Review comments as the contracts app serves them (Contracts app API v13); the app is the authority for quote, state and outdated.
import { apiFetch } from '../../api.ts';

/** Where a comment is made: a document pointer, a choice, the changes rows of a choice, or an intent entry. */
export type Anchor = { on: 'document'; ptr: string } | { on: 'choice'; id: string } | { on: 'change'; choice: string } | { on: 'guarantee' | 'nonGoal'; text: string; id?: string };
export type Principal = { kind: 'person'; id: string } | { kind: 'tailnet' };
export type CommentState = 'draft' | 'sent' | 'answered' | 'addressed' | 'resolved';
export type Comment = {
  id: string; artifact: number; version: number; anchor: Anchor; quote: string; body: string; author: Principal; state: CommentState; createdAt: string;
  sentAt?: string; review?: string; replies: Array<{ text: string; at: string }>; addressedIn?: number; outdated?: { mark: 'changed' | 'removed'; in: number }; resolvedAt?: string;
};
export type Review = { id: string; artifact: number; askId: string; note?: string; comments: string[]; by: Principal; preparedAt: string; state: 'prepared' | 'sent' | 'released'; reason: string };

/** A refusal from the app, with its status so a caller can tell a 409 from a 400. */
export class AppError extends Error {
  status: number;
  constructor(status: number, message: string) { super(message); this.status = status; }
}

const base = (key: string) => `apps/contracts/contracts/${encodeURIComponent(key)}`;
async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await apiFetch(path, { ...init, headers: { 'content-type': 'application/json' } });
  if (!res.ok) {
    const p = (await res.json().catch(() => null)) as { detail?: string } | null;
    throw new AppError(res.status, p?.detail ?? `HTTP ${res.status}`);
  }
  return (res.status === 204 ? undefined : await res.json()) as T;
}

/** Every comment the caller may see on one contract, and who the app takes the caller to be. */
export async function listComments(key: string): Promise<{ comments: Comment[]; caller: Principal }> {
  const comments: Comment[] = [];
  for (let token = ''; ;) {
    const page = await call<{ comments: Comment[]; caller: Principal; nextPageToken?: string }>(`${base(key)}/comments?pageSize=1000${token && `&pageToken=${encodeURIComponent(token)}`}`);
    comments.push(...page.comments);
    if (!page.nextPageToken) return { comments, caller: page.caller };
    token = page.nextPageToken;
  }
}

const post = (body: unknown) => ({ method: 'POST', body: JSON.stringify(body) });
export const createComment = (key: string, artifact: number, anchor: Anchor, body: string) => call<Comment>(`${base(key)}/comments`, post({ artifact, anchor, body }));
export const editComment = (key: string, id: string, body: string) => call<Comment>(`${base(key)}/comments/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify({ body }) });
export const deleteComment = (key: string, id: string) => call<void>(`${base(key)}/comments/${encodeURIComponent(id)}`, { method: 'DELETE' });
export const resolveComment = (key: string, id: string) => call<Comment>(`${base(key)}/comments/${encodeURIComponent(id)}:resolve`, { method: 'POST' });
export const prepareReview = (key: string, comments: string[], note?: string) => call<Review>(`${base(key)}/reviews`, post({ comments, ...(note && { note }) }));
export const getReview = (key: string, id: string) => call<Review>(`${base(key)}/reviews/${encodeURIComponent(id)}`);
export const releaseReview = (key: string, id: string) => call<void>(`${base(key)}/reviews/${encodeURIComponent(id)}`, { method: 'DELETE' });

/** One key per place, so an element finds its comments: an intent entry by its id when it has one. */
export function anchorKey(a: Anchor): string {
  if (a.on === 'document') return `document:${a.ptr}`;
  if (a.on === 'choice') return `choice:${a.id}`;
  if (a.on === 'change') return `change:${a.choice}`;
  return `${a.on}:${a.id ?? a.text}`;
}

export const isMe = (p: Principal, caller?: Principal) => !!caller && p.kind === caller.kind && (p.kind === 'tailnet' || p.id === (caller as { id: string }).id);
export const whoName = (p: Principal, caller?: Principal) => (isMe(p, caller) ? 'You' : p.kind === 'person' ? p.id : 'The tailnet');

// The block of the Contracts app API's review reason; JSON escapes newlines, so the block's line holds no fence.
const BLOCK = /(?:^|\n)```quack-review\n([^\n]*)\n```(?=\n|$)/g;

/** A rejection reason as a person reads it: a review's note and how many comments it sent, never its JSON; a plain reason as it is. */
export function readReason(reason: string): { note: string; comments?: number } {
  const last = [...reason.matchAll(BLOCK)].at(-1);
  let comments: unknown;
  try { comments = last && (JSON.parse(last[1]!) as { comments?: unknown }).comments; } catch { comments = undefined; }
  if (!last || !Array.isArray(comments)) return { note: reason };
  return { note: reason.slice(0, last.index).trim(), comments: comments.length };
}

/** The reason in one line of prose: "<note> · 3 comments". */
export function reasonText(reason: string): string {
  const r = readReason(reason);
  if (r.comments === undefined) return r.note;
  const n = `${r.comments} ${r.comments === 1 ? 'comment' : 'comments'}`;
  return r.note ? `${r.note}\n${n}` : n;
}
