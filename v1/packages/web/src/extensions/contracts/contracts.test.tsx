import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test } from 'vitest';
import type { SealedArtifact } from '../../api.ts';
import { contracts } from './index.ts';
import { fields, parseSpec, toYaml, type ContractSpec } from './spec.ts';

const raw = (v: number) => readFileSync(join(process.cwd(), 'src/extensions/contracts/fixtures', `sleeper-schedule-v${v}.json`), 'utf8');
const sealed = (content: string, over: Partial<SealedArtifact> = {}): SealedArtifact => ({ kind: 'contract.spec', version: 1, hash: 'h', content, sealedAt: 1, verdict: null, ...over });
const show = (v: number) => render(<>{contracts.card!(sealed(raw(v), { version: v }))}</>);

test('version 1 shows the route, its fields with type and required, and an example', () => {
  show(1);
  expect(screen.getByRole('heading', { name: 'Sleeper schedule endpoint' })).toBeInTheDocument();
  expect(screen.getByText('1 route · version 1')).toBeInTheDocument();
  expect(screen.getByText('/apps/sleeper/schedule')).toBeInTheDocument();
  const ok = screen.getByText('The week and its runs').closest('div.rounded-xl') as HTMLElement;
  const week = within(ok).getByText('week').closest('li') as HTMLElement;
  expect(week).toHaveTextContent('integer');
  expect(week).toHaveTextContent('REQUIRED');
  expect(week).toHaveTextContent('example9');
  expect(within(ok).getByText('at').closest('li')).toHaveTextContent('date-time');
  expect(within(ok).getByText('job').closest('li')).toHaveTextContent('lineup · waivers');
  expect(screen.getByText(/"week": 9/)).toBeInTheDocument();
  expect(screen.queryByRole('region', { name: 'What changed' })).toBeNull();
  expect(screen.queryByRole('region', { name: 'Compatibility' })).toBeNull();
});

test('a why chip opens the reasons for its own choice, by source', async () => {
  show(1);
  const ok = screen.getByText('503 Schedules service unreachable'.slice(4)).closest('div.rounded-xl') as HTMLElement;
  await userEvent.click(within(ok).getByRole('button', { name: 'Why: 503 when schedules is down' }));
  const panel = screen.getByRole('region', { name: 'Why: 503 when schedules is down' });
  const items = within(panel).getAllByRole('listitem').map((li) => li.textContent);
  expect(items).toEqual([
    'You"An error the page can show"question 3',
    'GuideA dependency outage is 503 with a retry hint, not an empty successREST guide, errors',
    'ExistingThe app proxy already returns 502 when Sleeper itself is downv1/packages/distributor/src/proxy.ts:88',
  ]);
  await userEvent.click(within(panel).getByRole('button', { name: 'Close' }));
  expect(screen.queryByRole('region', { name: /^Why:/ })).toBeNull();

  const week = screen.getByText('week').closest('li') as HTMLElement;
  await userEvent.click(within(week).getByRole('button', { name: 'Why: Live week from Sleeper itself' }));
  expect(screen.getByRole('region', { name: 'Why: Live week from Sleeper itself' })).toHaveTextContent('A week with no runs yet still shows');
});

test('every choice can be opened, and the ones on a field sit on that field', async () => {
  const spec = parseSpec(raw(2))!;
  show(2);
  for (const c of spec.choices) expect(screen.getAllByRole('button', { name: `Why: ${c.decision}` }).length).toBeGreaterThan(0);
  const status = screen.getByText('status').closest('li') as HTMLElement;
  expect(within(status).getByRole('button', { name: 'Why: Each run says whether it is done' })).toBeInTheDocument();
});

test('version 2 lists what changed, why, and the caller it breaks', async () => {
  show(2);
  const changes = screen.getByRole('region', { name: 'What changed' });
  expect(within(changes).getByText('Changed from version 1')).toBeInTheDocument();
  const rows = within(changes).getAllByRole('listitem');
  expect(rows).toHaveLength(3);
  expect(rows[0]).toHaveTextContent('Addedruns[].status: "done" | "next"Your request: mark runs that happened');
  expect(rows[1]).toHaveTextContent('Was upcoming only, soonest first');
  const compat = screen.getByRole('region', { name: 'Compatibility' });
  expect(within(compat).getByText('Breaks the current caller')).toBeInTheDocument();
  expect(within(compat).getByText('Sleeper page')).toBeInTheDocument();
  expect(within(compat).getByText('Still read-only')).toBeInTheDocument();
  await userEvent.click(within(rows[2]!).getByRole('button', { name: 'Why: Done runs link to their result' }));
  expect(screen.getByRole('region', { name: 'Why: Done runs link to their result' })).toHaveTextContent('v1/packages/web/src/extensions/sleeper/Sleeper.tsx:212');
});

test('several routes list under their tag and switch the detail', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec & { document: { paths: Record<string, unknown> } };
  spec.document.paths['/leagues/{league}/jobs/{job}'] = { post: { tags: ['Jobs'], summary: 'Starts one job.', parameters: [{ name: 'job', in: 'path', required: true, schema: { type: 'string' } }], responses: { '202': { description: 'Accepted' } } } };
  render(<>{contracts.card!(sealed(JSON.stringify(spec)))}</>);
  const nav = screen.getByRole('navigation', { name: 'Routes' });
  await userEvent.click(within(nav).getByRole('button', { name: /jobs/ }));
  expect(screen.getByText('Starts one job.')).toBeInTheDocument();
  expect(screen.getByText('path')).toBeInTheDocument();
});

test('only a readable contract.spec draws as a card', () => {
  expect(contracts.card!(sealed('not json'))).toBeUndefined();
  expect(contracts.card!(sealed(raw(1), { kind: 'github.review' }))).toBeUndefined();
  expect(contracts.artifact!(sealed(raw(1)), false)).toEqual({ name: 'Spec' });
});

test('the schema walk follows $ref and the YAML export is plain', () => {
  const doc = { components: { schemas: { Run: { type: 'object', required: ['at'], properties: { at: { type: 'string', format: 'date-time' } } } } } };
  expect(fields(doc, { type: 'array', items: { $ref: '#/components/schemas/Run' } }, '/x').map((f) => [f.name, f.type, f.required, f.ptr])).toEqual([['at', 'date-time', true, '/components/schemas/Run/properties/at']]);
  expect(toYaml({ a: 1, 'b/c': [{ d: 'x: y' }, []], e: {} })).toBe('a: 1\n"b/c":\n  -\n    d: "x: y"\n  - []\ne: {}');
});
