import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseCheckout } from './bench/reviewer-cases.ts';

test('a reviewer case reads the pull request back from the checkout_pr text the review saw', () => {
  const text = [
    'acme/widgets#7 by ann: feat: a flag',
    'Checked out abc in ./repo; diff against origin/main: git -C repo diff origin/main...HEAD',
    'Description:\nAdds a flag.\n\n## Why\nIt was missing.',
    'CI checks: check=success, isolation=in_progress',
    'Linked issue #3: flag\nwe need one\n\nreally',
    'Linked issue #4: docs\n',
    'Changed files:\n a.ts | 1 +',
  ].join('\n\n');
  assert.deepEqual(parseCheckout(text), {
    owner: 'acme', repo: 'widgets', number: 7, title: 'feat: a flag', body: 'Adds a flag.\n\n## Why\nIt was missing.', baseRef: 'main',
    checks: [{ name: 'check', status: 'completed', conclusion: 'success' }, { name: 'isolation', status: 'completed', conclusion: 'in_progress' }],
    linkedIssues: [{ number: 3, title: 'flag', body: 'we need one\n\nreally' }, { number: 4, title: 'docs', body: '' }],
  });
  assert.deepEqual(parseCheckout(text.replace('check=success, isolation=in_progress', 'none reported').replace('Adds a flag.\n\n## Why\nIt was missing.', '(none)')).checks, []);
});
