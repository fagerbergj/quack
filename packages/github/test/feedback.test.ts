import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ownThreads, renderOwnThreads } from '../src/feedback.ts';

const BOT = 'quack-app[bot]';
const at = (minute: number) => new Date(Date.UTC(2026, 10, 9, 22, minute)).toISOString();
const comment = (id: number, login: string, minute: number, extra: Record<string, unknown> = {}) => ({ id, user: { login }, path: 'src/a.ts', line: 16, commit_id: 'head2222', original_commit_id: 'old11111', created_at: at(minute), body: `comment ${id}`, ...extra });

test('own threads pair each of the app\'s own comments with its replies, newest thread first, and ignore other people\'s threads', () => {
  const comments = [
    comment(1, 'Quack-App[bot]', 0, { body: 'question: are the routes exposed?' }),
    comment(2, 'dev', 3, { in_reply_to_id: 1, body: 'Checked on prod: all 302 to the login.' }),
    comment(3, 'maria', 1, { body: 'Off by one.' }),
    comment(4, BOT, 2, { in_reply_to_id: 3, body: 'Agreed.' }),
    comment(5, BOT, 5, { line: null, body: 'nit: rename.' }),
  ];
  const { threads, cut } = ownThreads(comments, BOT);
  assert.equal(cut, 0);
  assert.deepEqual(threads.map((t) => [t.id, t.outdated, t.replies.map((r) => [r.author, r.at, r.body])]), [
    [5, true, []],
    [1, false, [['dev', at(3), 'Checked on prod: all 302 to the login.']]],
  ]);
  assert.equal(threads[1].commit, 'old11111', 'the commit it was made on, not the one GitHub moved it to');
});

test('own threads keep the newest threads and the newest replies of each, in order, and count what they cut', () => {
  const comments = [
    ...[1, 2, 3].map((id) => comment(id, BOT, id)),
    ...[10, 11, 12, 13].map((id) => comment(id, 'dev', id, { in_reply_to_id: 3 })),
  ];
  const { threads, cut } = ownThreads(comments, BOT, 2, 3);
  assert.deepEqual(threads.map((t) => t.id), [3, 2]);
  assert.equal(cut, 1);
  assert.deepEqual(threads[0].replies.map((r) => r.body), ['comment 11', 'comment 12', 'comment 13']);
  assert.equal(threads[0].repliesCut, 1);
  const text = renderOwnThreads({ reviews: [], comments: [], ownThreads: threads, ownThreadsCut: cut });
  assert.match(text, /### src\/a\.ts:16, commented on old11111 at .+; the commented line is unchanged in the current diff\ncomment 3\n- \(1 older replies not shown\)\n- reply by dev at /);
  assert.match(text, /### src\/a\.ts:16, .*\ncomment 2\n- no replies/);
  assert.match(text, /\(1 older comments of yours not shown\)$/);
});

test('threads past the size budget are counted as your comments', () => {
  const { threads } = ownThreads([1, 2].map((id) => comment(id, BOT, id, { body: 'x'.repeat(500) })), BOT);
  assert.match(renderOwnThreads({ reviews: [], comments: [], ownThreads: threads }, 700), /\(1 more of your comments cut to fit the size budget; read them on the pull request\)/);
});

test('a first review shows no thread section; a failed read says so', () => {
  assert.equal(renderOwnThreads({ reviews: [], comments: [], ownThreads: [] }), '');
  assert.equal(renderOwnThreads(undefined), '');
  assert.match(renderOwnThreads({ reviews: [], comments: [], ownThreadsError: 'boom' }), /could not be read: boom/);
});
