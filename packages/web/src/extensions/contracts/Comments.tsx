import { createContext, useContext, useId, useMemo, useState, type ReactNode } from 'react';
import { decideApproval, type Approval } from '../../api.ts';
import { MoreIcon } from '../../MoreIcon.tsx';
import { useMedia } from '../../useDesktop.ts';
import { useLoad } from '../../useLoad.ts';
import { useMenu } from '../../useMenu.ts';
import { prim, sec } from './Approval.tsx';
import { anchorKey, AppError, readReason, createComment, deleteComment, editComment, getReview, isMe, listComments, prepareReview, releaseReview, resolveComment, whoName, type Anchor, type Comment, type Principal, type Review } from './comments.ts';
import { Sheet } from './Review.tsx';

/** A place a person can comment on, as the page names it: `tag` says which part of the page, `text` what it says there. */
export type Target = { anchor: Anchor; tag: string; name: string; text?: string };

type Open = { kind: 'new'; target: Target } | { kind: 'thread'; target: Target } | { kind: 'review' } | { kind: 'all' } | { kind: 'stranded' };
type Ctx = {
  /** Set while a version waits on the person: the only one that takes comments. */
  pending?: { artifact: number; name: string; proposal: Approval };
  byKey: Map<string, Comment[]>; all: Comment[]; drafts: Comment[]; caller?: Principal;
  /** The caller's drafts on a version no longer up for approval: they cannot be sent, so the page offers to remove or move them. */
  stranded: Comment[]; newest: number;
  open: (o: Open) => void; reload: () => void; versionName: (artifact: number) => string;
};
const NONE: Ctx = { byKey: new Map(), all: [], drafts: [], stranded: [], newest: 0, open: () => {}, reload: () => {}, versionName: (n) => `#${n}` };
const CommentsContext = createContext<Ctx>(NONE);
export const useComments = () => useContext(CommentsContext);

const EMPTY: { comments: Comment[]; caller?: Principal } = { comments: [] };

/** The comments on one contract and the sheets that make, send and read them; `deps` reload them as the session moves. */
export function CommentsProvider({ sessionKey, pending, newest, versionName, decided, deps, children }: { sessionKey: string; pending?: Ctx['pending']; newest: number; versionName: Ctx['versionName']; decided: (id: string) => void; deps: unknown[]; children: ReactNode }) {
  const { data, reload } = useLoad(() => listComments(sessionKey).catch(() => EMPTY), [sessionKey, ...deps], EMPTY);
  const [shown, open] = useState<Open>();
  const value = useMemo<Ctx>(() => {
    const byKey = Map.groupBy(data.comments, (c) => anchorKey(c.anchor));
    const mine = data.comments.filter((c) => c.state === 'draft' && isMe(c.author, data.caller));
    const drafts = mine.filter((c) => c.artifact === pending?.artifact);
    const stranded = mine.filter((c) => c.artifact !== pending?.artifact);
    return { pending, byKey, all: data.comments, drafts, stranded, newest, caller: data.caller, open, reload, versionName };
  }, [data, pending, newest, versionName, reload]);
  const close = () => open(undefined);
  return (
    <CommentsContext value={value}>
      {children}
      {shown?.kind === 'new' && pending && <NewComment sessionKey={sessionKey} target={shown.target} onClose={close} />}
      {shown?.kind === 'thread' && <Thread sessionKey={sessionKey} title={`Comments on ${shown.target.name}`} comments={value.byKey.get(anchorKey(shown.target.anchor)) ?? []} onClose={close} />}
      {shown?.kind === 'stranded' && <Thread sessionKey={sessionKey} title="Drafts that can't be sent" comments={value.stranded} onClose={close} />}
      {shown?.kind === 'all' && <Thread sessionKey={sessionKey} title="Review comments" comments={value.all.filter((c) => c.state !== 'draft')} onClose={close} />}
      {shown?.kind === 'review' && pending && <ReviewSheet sessionKey={sessionKey} decided={decided} onClose={close} />}
    </CommentsContext>
  );
}

const icon = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-muted hover:bg-soft hover:text-link focus-visible:outline-2 focus-visible:outline-link';
const BUBBLE = 'M5 5h14v10H10l-4 4v-4H5z';
const bubble = <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" aria-hidden="true"><path d={BUBBLE} /></svg>;

/** A small filled speech bubble, the mark a commented element carries. */
export const CommentGlyph = () => <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d={BUBBLE} /></svg>;
/** The comments on a declaration or anything under it, as a diagram box counts them. */
export const commentsUnder = (all: Comment[], ptr: string) => all.filter((c) => c.anchor.on === 'document' && (c.anchor.ptr === ptr || c.anchor.ptr.startsWith(`${ptr}/`)));

// The comments already on an element, as a count that opens their thread.
function Marker({ target, n }: { target: Target; n: number }) {
  const { open } = useComments();
  return (
    <button type="button" aria-label={`${n} ${n === 1 ? 'comment' : 'comments'} on ${target.name}`} onClick={() => open({ kind: 'thread', target })} className="-my-2 inline-flex min-h-11 min-w-11 flex-none cursor-pointer items-center justify-center gap-1 rounded-full border-0 bg-transparent px-1 text-[12px] font-semibold text-link hover:bg-soft focus-visible:outline-2 focus-visible:outline-link">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d={BUBBLE} /></svg>{n}
    </button>
  );
}

// On a touch screen the action sits in a row menu, so it never hides behind a long press.
function RowMenu({ target }: { target: Target }) {
  const { open } = useComments();
  const menu = useMenu();
  return (
    <div ref={menu.root} onKeyDown={menu.keys} className="relative -my-2 flex-none">
      <button ref={menu.button} type="button" aria-label={`More for ${target.name}`} aria-haspopup="menu" aria-expanded={menu.open} onClick={menu.toggle} className={icon}><MoreIcon /></button>
      {menu.open && (
        <div role="menu" aria-label={`More for ${target.name}`} className="absolute right-0 z-20 mt-1 min-w-44 rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          <button type="button" role="menuitem" onClick={() => (menu.close(), open({ kind: 'new', target }))} className="flex min-h-11 w-full cursor-pointer items-center gap-2 border-0 bg-transparent px-3 text-left text-[14px] text-ink hover:bg-soft focus-visible:bg-soft focus-visible:outline-none">{bubble}Comment</button>
        </div>
      )}
    </div>
  );
}

/** An element's comment marker and, while a version waits on the person, its Comment action: an icon that shows on hover or focus with a
 * mouse, a row menu on a touch screen. Its row carries the `group/c` class. */
export function CommentButton({ target }: { target: Target }) {
  const { pending, byKey } = useComments();
  const touch = useMedia('(pointer: coarse)');
  const n = byKey.get(anchorKey(target.anchor))?.length ?? 0;
  const action = pending && (touch ? <RowMenu target={target} /> : <CommentIcon target={target} />);
  if (!n && !action) return null;
  return <span className="-my-2 ml-1 inline-flex flex-none items-center align-middle">{n > 0 && <Marker target={target} n={n} />}{action}</span>;
}

// Hidden until its row (the nearest `group/c`) is pointed at or holds focus; it is in the tab order all along.
function CommentIcon({ target }: { target: Target }) {
  const { open } = useComments();
  return <button type="button" aria-label={`Comment on ${target.name}`} title="Comment" onClick={() => open({ kind: 'new', target })} className={`${icon} opacity-0 group-hover/c:opacity-100 group-focus-within/c:opacity-100 focus:opacity-100`}>{bubble}</button>;
}

/** The diagram strip's own Comment button, always shown: the strip is what a tap on a box opens. */
export function StripComment({ target }: { target: Target }) {
  const { pending, byKey, open } = useComments();
  const n = byKey.get(anchorKey(target.anchor))?.length ?? 0;
  const btn = 'flex min-h-11 cursor-pointer items-center gap-2 rounded-full border border-line bg-surface px-4 text-[14px] font-semibold text-ink hover:bg-soft focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link';
  return (
    <>
      {pending && <button type="button" onClick={() => open({ kind: 'new', target })} className={btn}>{bubble}Comment</button>}
      {n > 0 && <button type="button" onClick={() => open({ kind: 'thread', target })} className={btn}>{n} {n === 1 ? 'comment' : 'comments'}</button>}
    </>
  );
}

const field = 'focus-ring rounded-lg border border-line bg-surface px-3 py-2 text-[15px] text-ink';
const tagCls = 'flex-none rounded bg-soft px-1.5 py-0.5 font-mono text-[11px] tracking-wide text-muted uppercase';

function What({ tag, name, text }: { tag: string; name: string; text?: string }) {
  return (
    <div className="flex gap-3 rounded-xl bg-soft/60 p-3">
      <span className={`${tagCls} self-start`}>{tag}</span>
      <span className="flex min-w-0 flex-col gap-0.5 [overflow-wrap:anywhere]"><code className="font-mono text-[13px] font-semibold text-ink">{name}</code>{text && <span className="text-[13.5px] text-ink-2">{text}</span>}</span>
    </div>
  );
}

const MAX_BODY = 1000;
const errorOf = (e: unknown, name: string) => (e instanceof AppError && e.status === 409 ? `${name} is no longer up for approval, so it takes no comments: ${e.message}` : e instanceof Error ? e.message : String(e));

function NewComment({ sessionKey, target, onClose }: { sessionKey: string; target: Target; onClose: () => void }) {
  const { pending, reload } = useComments();
  const [body, setBody] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const form = useId();
  const add = async () => {
    setBusy(true);
    setError('');
    try {
      await createComment(sessionKey, pending!.artifact, target.anchor, body.trim());
      reload();
      onClose();
    } catch (e) {
      setError(errorOf(e, pending!.name));
    } finally {
      setBusy(false);
    }
  };
  const long = [...body].length > MAX_BODY;
  const actions = <><button type="button" onClick={onClose} className={sec}>Cancel</button><button type="submit" form={form} disabled={busy || !body.trim() || long} className={prim}>{busy ? 'Adding…' : 'Add to review'}</button></>;
  return (
    <Sheet title="Comment" onClose={onClose} actions={actions}>
      <What tag={target.tag} name={target.name} text={target.text} />
      <form id={form} className="flex flex-col gap-2" onSubmit={(e) => (e.preventDefault(), void add())}>
        <label className="flex flex-col gap-1 text-[14px] font-semibold text-ink">
          Your comment
          <textarea required data-autofocus value={body} onChange={(e) => setBody(e.target.value)} rows={4} aria-describedby={`${form}-note`} className={field} />
        </label>
        {long && <p role="alert" className="m-0 text-[13px] text-danger">A comment is at most {MAX_BODY} characters; this one has {[...body].length}.</p>}
        <p id={`${form}-note`} className="m-0 text-[13px] text-muted">It waits in your review of {pending!.name} until you send the review, which asks for changes to {pending!.name}.</p>
        {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      </form>
    </Sheet>
  );
}

const SEGMENT_SKIP = new Set(['properties', 'items', 'content', 'application/json', 'schema', 'components', 'schemas', '$defs']);
/** How the page names an anchor where the element is not at hand: a route, a field path, a decision or an intent entry. */
export function anchorName(a: Anchor): { tag: string; name: string } {
  if (a.on === 'choice') return { tag: 'Decision', name: a.id };
  if (a.on === 'change') return { tag: 'Change', name: a.choice };
  if (a.on !== 'document') return { tag: a.on === 'guarantee' ? 'Guarantee' : 'Out of scope', name: a.id ?? a.text };
  const s = a.ptr.split('/').slice(1).map((x) => x.replaceAll('~1', '/').replaceAll('~0', '~'));
  if (s[0] === 'paths' && s.length >= 3) return { tag: 'Spec', name: [`${s[2]!.toUpperCase()} ${s[1]}`, ...s.slice(3).filter((x) => !SEGMENT_SKIP.has(x))].join(' › ') };
  return { tag: 'Spec', name: s.filter((x) => !SEGMENT_SKIP.has(x)).join('.') || 'The whole document' };
}

function Draft({ sessionKey, c, held, checked, onToggle }: { sessionKey: string; c: Comment; held: boolean; checked?: boolean; onToggle?: () => void }) {
  const { reload } = useComments();
  const [editing, setEditing] = useState<string>();
  const [error, setError] = useState('');
  const { tag, name } = anchorName(c.anchor);
  const run = (p: Promise<unknown>) => void p.then(() => (setEditing(undefined), reload()), (e: Error) => setError(e.message));
  const link = 'min-h-11 cursor-pointer border-0 bg-transparent px-2 text-[14px] font-semibold text-link hover:underline focus-ring';
  return (
    <li className="flex flex-col gap-1.5 border-t border-line py-3 first:border-t-0">
      <div className="flex items-center gap-2">
        {onToggle && <input type="checkbox" checked={checked} onChange={onToggle} aria-label={`Include comment on ${name}`} className="h-4 w-4 flex-none cursor-pointer accent-link" />}
        <span className={tagCls}>{tag}</span><code className="min-w-0 flex-1 font-mono text-[13px] font-semibold [overflow-wrap:anywhere]">{name}</code>
        {!held && editing === undefined && <><button type="button" aria-label={`Edit comment on ${name}`} onClick={() => setEditing(c.body)} className={link}>Edit</button><button type="button" aria-label={`Remove comment on ${name}`} onClick={() => run(deleteComment(sessionKey, c.id))} className={link}>Remove</button></>}
      </div>
      {editing === undefined ? <p className="m-0 whitespace-pre-wrap [overflow-wrap:anywhere]">{c.body}</p> : (
        <form className="flex flex-col gap-2" onSubmit={(e) => (e.preventDefault(), run(editComment(sessionKey, c.id, editing.trim())))}>
          <textarea aria-label={`Comment on ${name}`} data-autofocus value={editing} onChange={(e) => setEditing(e.target.value)} rows={3} className={field} />
          <span className="flex justify-end gap-2"><button type="button" onClick={() => setEditing(undefined)} className={sec}>Cancel</button><button type="submit" disabled={!editing.trim()} className={prim}>Save</button></span>
        </form>
      )}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </li>
  );
}

type Stuck = { review: Review; why: string };

// Prepares the review (or reads back the one a failed send left held) and rejects the ask with its reason, verbatim.
function useSend(sessionKey: string, decided: (id: string) => void, onClose: () => void) {
  const { pending, reload } = useComments();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [stuck, setStuck] = useState<Stuck>();
  const reject = async (review: Review) => {
    try {
      if (!(await decideApproval(sessionKey, pending!.proposal.id, 'reject', review.reason))) return setError(`${pending!.name} was already decided, in another tab or by a message you sent, so your review was not sent; your comments are drafts again.`);
      decided(pending!.proposal.id);
      onClose();
    } catch (e) {
      const why = (e as Error).message;
      if (why.startsWith('the request was withdrawn')) setError(`${pending!.name} was replaced before your review was sent, so it was not sent; your comments are drafts again. ${why}`);
      else setStuck({ review, why });
    }
  };
  const go = async (start: () => Promise<Review>) => {
    setBusy(true);
    setError('');
    setStuck(undefined);
    try {
      await reject(await start());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
      reload();
    }
  };
  return {
    busy, error, stuck,
    send: (ids: string[], note: string) => go(() => prepareReview(sessionKey, ids, note.trim() || undefined)),
    resend: (id: string) => go(() => getReview(sessionKey, id)),
    release: (id: string) => go(async () => { await releaseReview(sessionKey, id); throw new Error('Your review was released; its comments are drafts again.'); }),
  };
}

const MAX_PER_REVIEW = 20;

function useDraftSelection(drafts: Comment[], pick: boolean) {
  const [selected, setSelected] = useState<Set<string>>(() => new Set(drafts.slice(0, MAX_PER_REVIEW).map((c) => c.id)));
  const toggle = (id: string) => setSelected((prev) => {
    const next = new Set(prev);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    return next;
  });
  const chosen = pick ? drafts.filter((c) => selected.has(c.id)).map((c) => c.id) : drafts.map((c) => c.id);
  return { selected, chosen, toggle };
}

function ReviewLimitNote({ count, total }: { count: number; total: number }) {
  const over = count > MAX_PER_REVIEW;
  return <p role={over ? 'alert' : undefined} className={`m-0 text-[13px] ${over ? 'text-danger' : 'text-warning'}`}>A review sends at most {MAX_PER_REVIEW} comments; choose which to include ({count} of {total} selected).</p>;
}

function ReviewActions({ held, busy, count, words, onClose, onRelease, onResend, onSend }: { held?: string; busy: boolean; count: number; words: string; onClose: () => void; onRelease: (id: string) => void; onResend: (id: string) => void; onSend: () => void }) {
  if (held) return <><button type="button" disabled={busy} onClick={() => onRelease(held)} className={sec}>Release</button><button type="button" disabled={busy} onClick={() => onResend(held)} className={prim}>{busy ? 'Sending…' : 'Send again'}</button></>;
  return <><button type="button" onClick={onClose} className={sec}>Keep editing</button><button type="button" disabled={busy || !count || count > MAX_PER_REVIEW} onClick={onSend} className={prim}>{busy ? 'Sending…' : `Send ${words}`}</button></>;
}

function ReviewSheet({ sessionKey, decided, onClose }: { sessionKey: string; decided: (id: string) => void; onClose: () => void }) {
  const { pending, drafts } = useComments();
  const [note, setNote] = useState('');
  const s = useSend(sessionKey, decided, onClose);
  const held = s.stuck?.review.id ?? drafts.find((c) => c.review)?.review;
  const n = drafts.length;
  const pick = !held && n > MAX_PER_REVIEW;
  const { selected, chosen, toggle } = useDraftSelection(drafts, pick);
  const count = chosen.length;
  const words = `${count} ${count === 1 ? 'comment' : 'comments'}`;
  const actions = <ReviewActions held={held} busy={s.busy} count={count} words={words} onClose={onClose} onRelease={(id) => void s.release(id)} onResend={(id) => void s.resend(id)} onSend={() => void s.send(chosen, note)} />;
  return (
    <Sheet title={`Your review of ${pending!.name}`} onClose={onClose} actions={actions}>
      {held ? <p role="status" className="m-0 rounded-xl bg-warning-bg px-3 py-2 text-warning">Your review is ready but was not sent{s.stuck ? `: ${s.stuck.why}` : ''}. Send it again, or release it to edit its comments.</p>
        : <p className="m-0 text-ink-2">{n ? `${words}. Sending them asks for changes to ${pending!.name}; each stays attached to what it is about.` : 'No draft comments. Comment on a part of the diagram or the spec first.'}</p>}
      {pick && <ReviewLimitNote count={count} total={n} />}
      {n > 0 && <ul className="m-0 list-none p-0">{drafts.map((c) => <Draft key={c.id} sessionKey={sessionKey} c={c} held={!!held} checked={selected.has(c.id)} onToggle={pick ? () => toggle(c.id) : undefined} />)}</ul>}
      {!held && n > 0 && (
        <label className="flex flex-col gap-1 text-[14px] font-semibold text-ink">
          Overall note (optional)
          <textarea value={note} onChange={(e) => setNote(e.target.value)} rows={2} placeholder="Anything that is not about one part" className={field} />
        </label>
      )}
      {s.error && <p role="alert" className="m-0 text-[13px] text-danger">{s.error}</p>}
    </Sheet>
  );
}

const time = (iso: string) => new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
const STATE: Record<Comment['state'], [string, string]> = { draft: ['Draft', 'bg-soft text-muted'], sent: ['Sent', 'bg-link-bg text-link'], answered: ['Answered', 'bg-link-bg text-link'], addressed: ['Addressed', 'bg-success-bg text-success'], resolved: ['Resolved', 'bg-soft text-muted'] };
const chip = 'rounded px-1.5 py-0.5 text-[12px] font-semibold';

// Why a stranded draft cannot be sent: a newer version replaced its own, or its own is no longer up for approval.
const strandedWhy = (c: Comment, newest: number, name: string) => `${name} ${c.artifact < newest ? 'was replaced' : 'is no longer up for approval'}, so this can't be sent`;

/** A draft on a version no longer up for approval: Remove it, or move it to the version that is, by the same anchor. */
function Stranded({ sessionKey, c }: { sessionKey: string; c: Comment }) {
  const { pending, newest, versionName, reload } = useComments();
  const [error, setError] = useState('');
  const run = (p: Promise<unknown>) => void p.then(reload, (e: unknown) => setError(e instanceof AppError && e.status === 400 ? `What this draft is about is not in ${pending?.name}, so it stays on ${versionName(c.artifact)}. Remove it, or copy its text into a new comment.` : (e as Error).message));
  const move = async () => { await createComment(sessionKey, pending!.artifact, c.anchor, c.body); await deleteComment(sessionKey, c.id); };
  return (
    <>
      <p className="m-0 text-[13px] text-warning">Draft on {versionName(c.artifact)} · {strandedWhy(c, newest, versionName(c.artifact))}</p>
      <span className="flex flex-wrap gap-2">
        <button type="button" onClick={() => run(deleteComment(sessionKey, c.id))} className={sec}>Remove</button>
        {pending && <button type="button" onClick={() => run(move())} className={sec}>Move to {pending.name}</button>}
      </span>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </>
  );
}

function CommentCard({ sessionKey, c }: { sessionKey: string; c: Comment }) {
  const { caller, versionName, reload, stranded } = useComments();
  const [error, setError] = useState('');
  const { tag, name } = anchorName(c.anchor);
  const [stateName, stateCls] = STATE[c.state];
  const resolvable = isMe(c.author, caller) && (c.state === 'sent' || c.state === 'answered' || c.state === 'addressed');
  return (
    <li className="flex flex-col gap-2 rounded-xl border border-line p-3">
      <div className="flex flex-wrap items-center gap-2"><code className="font-mono text-[13px] font-semibold [overflow-wrap:anywhere]">{name}</code><span className={tagCls}>{tag} · on {versionName(c.artifact)}</span></div>
      <div className="flex flex-wrap gap-1.5">
        <span className={`${chip} ${stateCls}`}>{c.addressedIn ? `Addressed in ${versionName(c.addressedIn)}` : stateName}</span>
        {c.outdated && <span className={`${chip} bg-warning-bg text-warning`}>Outdated: {c.outdated.mark} in {versionName(c.outdated.in)}</span>}
        {c.state === 'resolved' && c.addressedIn && <span className={`${chip} bg-soft text-muted`}>Resolved</span>}
      </div>
      {c.outdated && <blockquote className="m-0 border-l-2 border-line pl-2 text-[13px] whitespace-pre-wrap text-muted [overflow-wrap:anywhere]">On {versionName(c.artifact)}: {c.quote}</blockquote>}
      <div className="flex flex-col gap-0.5"><span className="text-[12.5px] text-muted"><strong className="font-semibold text-ink">{whoName(c.author, caller)}</strong> · {time(c.createdAt)}</span><p className="m-0 whitespace-pre-wrap [overflow-wrap:anywhere]">{c.body}</p></div>
      {c.replies.map((r, i) => <div key={i} className="flex flex-col gap-0.5 border-l-2 border-success pl-2"><span className="text-[12.5px] text-muted"><strong className="font-semibold text-ink">Agent</strong> · {time(r.at)}</span><p className="m-0 whitespace-pre-wrap [overflow-wrap:anywhere]">{r.text}</p></div>)}
      {stranded.includes(c) && <Stranded sessionKey={sessionKey} c={c} />}
      {resolvable && <button type="button" onClick={() => void resolveComment(sessionKey, c.id).then(reload, (e: Error) => setError(e.message))} className={`${sec} self-start`}>Resolve</button>}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </li>
  );
}

function Thread({ sessionKey, title, comments, onClose }: { sessionKey: string; title: string; comments: Comment[]; onClose: () => void }) {
  return (
    <Sheet title={title} onClose={onClose}>
      {comments.length ? <ul className="m-0 flex list-none flex-col gap-3 p-0">{comments.map((c) => <CommentCard key={c.id} sessionKey={sessionKey} c={c} />)}</ul> : <p className="m-0 text-muted">No comments here any more.</p>}
    </Sheet>
  );
}

/** The decision bar's line for drafts in hand: how many, and whether a review of them is ready but not sent. */
export function DraftsLine() {
  const { drafts } = useComments();
  const n = drafts.length;
  if (!n) return null;
  return <p className="m-0 flex items-center gap-1.5 text-[13px] font-semibold text-warning">{bubble}{n} draft {n === 1 ? 'comment' : 'comments'}{drafts.some((c) => c.review) ? ', ready but not sent' : ''}</p>;
}

/** The decision bar's notice for drafts that can no longer be sent, which opens them to remove or move; absent while there are none. */
export function StrandedLine() {
  const { stranded, newest, open } = useComments();
  const n = stranded.length;
  if (!n) return null;
  const where = stranded.every((c) => c.artifact < newest) ? 'a replaced version' : 'a version no longer up for approval';
  return <button type="button" onClick={() => open({ kind: 'stranded' })} className="min-h-11 cursor-pointer self-start border-0 bg-transparent p-0 text-left text-[13px] font-semibold text-link underline-offset-2 hover:underline focus-ring">{n} {n === 1 ? 'draft is' : 'drafts are'} on {where}</button>;
}

/** The More about list's row for the review comments already sent, which opens all their threads; absent while there are none. */
export function CommentsRow() {
  const { all, open } = useComments();
  const n = all.filter((c) => c.state !== 'draft').length;
  if (!n) return null;
  return (
    <li>
      <button type="button" onClick={() => open({ kind: 'all' })} className="flex min-h-11 w-full cursor-pointer items-center gap-2.5 border-0 bg-transparent px-3 text-left text-[14px] text-ink hover:bg-soft focus-ring">
        <span className="min-w-0 flex-1">Review comments</span>
        <span className="rounded-full bg-soft px-2 text-[12px] font-semibold text-ink-2 tabular-nums">{n}</span>
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true" className="text-muted"><path d="M9 6l6 6-6 6" /></svg>
      </button>
    </li>
  );
}

/** A spec element's target by its pointer, named as the page names it anywhere else. */
export const docTarget = (ptr: string, text?: string, tag = 'Spec'): Target => ({ anchor: { on: 'document', ptr }, tag, name: anchorName({ on: 'document', ptr }).name, ...(text && { text }) });

/** A changes-requested record as a person reads it: the version, the reason's note and how many review comments went with it. */
export function ChangesAsked({ content }: { content: string }) {
  let record: { version?: number; note?: string } = {};
  try { record = JSON.parse(content) as typeof record; } catch { /* shows what it can */ }
  const r = readReason(record.note ?? '');
  return (
    <div className="flex flex-col gap-2 rounded-[22px] border border-line bg-surface p-5 text-sm leading-relaxed text-ink-2 shadow-card [overflow-wrap:anywhere]">
      <p className="m-0 font-semibold text-ink">Changes asked for v{record.version ?? '?'}</p>
      {r.note && <p className="m-0 whitespace-pre-wrap">{r.note}</p>}
      {r.comments !== undefined && <p className="m-0 flex items-center gap-1.5 text-muted">{bubble}{r.comments} review {r.comments === 1 ? 'comment' : 'comments'}</p>}
    </div>
  );
}
