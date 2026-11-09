import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { artifactCard } from '../../Artifacts.tsx';
import { ArtifactPage } from '../../ArtifactPage.tsx';
import { delivered, versionOf } from '../../delivered.ts';
import { SessionKeyContext } from '../../extension.ts';
import type { SealedArtifact } from '../../api.ts';
import { contracts } from './index.ts';
import v1 from './fixtures/sleeper-schedule-v1.json';
import v2 from './fixtures/sleeper-schedule-v2.json';
import settingsSpec from './fixtures/sleeper-settings.json';
import { fields, parseSpec, toYaml, type ContractSpec } from './spec.ts';

const raw = (v: number) => JSON.stringify(v === 1 ? v1 : v2);
const sealed = (content: string, over: Partial<SealedArtifact> = {}): SealedArtifact => ({ kind: 'contract.spec', version: 1, hash: 'h', content, sealedAt: 1, verdict: null, ...over });
const show = (v: number) => render(<>{contracts.full!(sealed(raw(v), { version: v }))}</>);

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

test('an info button opens the reasons for its own choice, by source, and Escape closes it', async () => {
  show(1);
  const ok = screen.getByText('Schedules service unreachable').closest('div.rounded-xl') as HTMLElement;
  await userEvent.click(within(ok).getByRole('button', { name: 'Why: 503 when schedules is down' }));
  const panel = screen.getByRole('dialog', { name: 'Why: 503 when schedules is down' });
  const items = within(panel).getAllByRole('listitem').map((li) => li.textContent);
  expect(items).toEqual([
    'YouAnswered “What should the page get when the schedules service is down?”An error the page can show',
    'GuideA dependency outage is 503 with a retry hint, not an empty successREST guide, errors',
    'ExistingThe app proxy already returns 502 when Sleeper itself is downv1/packages/distributor/src/proxy.ts:88',
  ]);
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();

  const week = screen.getByText('week').closest('li') as HTMLElement;
  await userEvent.click(within(week).getByRole('button', { name: 'Why: Live week from Sleeper itself' }));
  expect(screen.getByRole('dialog', { name: 'Why: Live week from Sleeper itself' })).toHaveTextContent('Yes, always');
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
  expect(screen.getByRole('dialog', { name: 'Why: Done runs link to their result' })).toHaveTextContent('v1/packages/web/src/extensions/sleeper/Sleeper.tsx:212');
});

test('several routes list under their tag and switch the detail', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec & { document: { paths: Record<string, unknown> } };
  spec.document.paths['/leagues/{league}/jobs/{job}'] = { post: { tags: ['Jobs'], summary: 'Starts one job.', parameters: [{ name: 'job', in: 'path', required: true, schema: { type: 'string' } }], responses: { '202': { description: 'Accepted' } } } };
  render(<>{contracts.full!(sealed(JSON.stringify(spec)))}</>);
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

test('an answer shows the question as asked and the user\'s answer', async () => {
  show(1);
  await userEvent.click(screen.getAllByRole('button', { name: 'Why: GET with no body' })[0]!);
  const panel = screen.getByRole('dialog', { name: 'Why: GET with no body' });
  expect(within(panel).getByText('Answered “Should this ever start a job?”')).toBeInTheDocument();
  expect(within(panel).getByText('No, it only reads')).toBeInTheDocument();
});

test('the card links to its own page for its session; the page has no such link and offers versions', async () => {
  render(<SessionKeyContext value="home:main">{contracts.card!(sealed(raw(1)))}</SessionKeyContext>);
  expect(screen.getByRole('link', { name: 'Open full view' })).toHaveAttribute('href', '#/s/home%3Amain/a/contract.spec?v=1');
  const list = [sealed(raw(1)), sealed(raw(2), { version: 2 })];
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ artifacts: list }))));
  location.hash = '#/artifacts';
  render(<ArtifactPage sessionKey="home:main" kind="contract.spec" version={1} rows={undefined} />);
  expect(await screen.findByText('1 route · version 1')).toBeInTheDocument();
  expect(screen.getAllByRole('link', { name: 'Open full view' })).toHaveLength(1);
  expect(screen.getByRole('link', { name: 'Open its session' })).toHaveAttribute('href', '#/s/home%3Amain');
  await userEvent.click(screen.getByRole('button', { name: 'Version: Version 1' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Version 2' }));
  expect(location.hash).toBe('#/s/home%3Amain/a/contract.spec?v=2');
  vi.unstubAllGlobals();
});

test('a rejected attempt shares its spec version: the picker and page follow content.version and show the delivered seal', async () => {
  const ok = { passed: true, gatesFailed: [], score: 1, checks: [] };
  const draft = JSON.stringify({ ...JSON.parse(raw(2)), title: 'Rejected draft' });
  const list = [sealed(raw(1), { version: 1, verdict: ok }), sealed(draft, { version: 2, verdict: { ...ok, passed: false } }), sealed(raw(2), { version: 3, verdict: ok })];
  expect(delivered(list).map((a) => [a.version, versionOf(a)])).toEqual([[3, 2], [1, 1]]);
  expect(delivered(list.slice(0, 2)).map((a) => a.version)).toEqual([2, 1]);
  const released = [sealed(raw(2), { version: 2, verdict: { ...ok, passed: false, limitReached: true } }), sealed(draft, { version: 3, verdict: { ...ok, passed: false } })];
  expect(delivered(released).map((a) => a.version)).toEqual([2]);
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ artifacts: list }))));
  render(<ArtifactPage sessionKey="home:main" kind="contract.spec" version={2} rows={undefined} />);
  expect(await screen.findByText('1 route · version 2')).toBeInTheDocument();
  expect(screen.queryByText('Rejected draft')).toBeNull();
  await userEvent.click(screen.getByRole('button', { name: 'Version: Version 2' }));
  expect(screen.getAllByRole('menuitem').map((o) => o.textContent)).toEqual(['Version 2', 'Version 1']);
  vi.unstubAllGlobals();
});

test('a $ref that points back to itself, or round a loop, ends in a named reference', () => {
  const doc = { components: { schemas: {
    Node: { type: 'object', properties: { value: { type: 'string' }, next: { $ref: '#/components/schemas/Node' } } },
    A: { type: 'object', properties: { b: { $ref: '#/components/schemas/B' } } },
    B: { type: 'object', properties: { a: { $ref: '#/components/schemas/A' } } },
    List: { type: 'array', items: { $ref: '#/components/schemas/List' } },
    X: { $ref: '#/components/schemas/Y' },
    Y: { $ref: '#/components/schemas/X' },
  } } };
  const rows = (name: string) => fields(doc, { $ref: `#/components/schemas/${name}` }, '/x').map((f) => [f.name, f.type, f.desc]);
  expect(rows('Node')).toEqual([['value', 'string', undefined], ['next', 'Node', 'Node, see above']]);
  expect(rows('A')).toEqual([['b', 'object', undefined], ['a', 'A', 'A, see above']]);
  expect(rows('List')).toEqual([['each item', 'List', 'List, see above']]);
  expect(rows('X')).toEqual([]);
});

test('a spec without a usable document is not drawn, and one that throws while drawing falls back to its saved text', () => {
  const bad = (document: unknown) => JSON.stringify({ ...v1, document });
  for (const d of [null, 'x', {}, { paths: null }]) expect(parseSpec(bad(d))).toBeUndefined();
  const broken = JSON.stringify({ ...v1, intent: { ...v1.intent, purpose: 5 } });
  vi.spyOn(console, 'error').mockImplementation(() => undefined);
  render(<>{artifactCard(sealed(broken))}</>);
  expect(screen.getByRole('alert')).toHaveTextContent("This couldn't be drawn");
  expect(screen.getByRole('alert')).toHaveTextContent('purpose5');
  vi.restoreAllMocks();
});

test('the ledger card is compact: routes and a decisions summary, no detail or decisions list', () => {
  render(<>{contracts.card!(sealed(raw(1)))}</>);
  expect(screen.getByRole('heading', { name: 'Sleeper schedule endpoint' })).toBeInTheDocument();
  expect(screen.getByText('1 route · version 1')).toBeInTheDocument();
  expect(within(screen.getByRole('list', { name: 'Routes' })).getByText('/apps/sleeper/schedule')).toBeInTheDocument();
  expect(screen.getByText(/^\d+ decisions · \d+ answered by you$/)).toBeInTheDocument();
  expect(screen.getByRole('region', { name: 'What it does' })).toBeInTheDocument();
  expect(screen.queryByRole('region', { name: 'Decisions' })).toBeNull();
  expect(screen.queryByText('Responses')).toBeNull();
  expect(screen.getByRole('button', { name: 'Download OpenAPI file' })).toBeInTheDocument();
});

test('the full view keeps the operation detail, with no decisions list beside it', () => {
  show(1);
  expect(screen.getByText('Responses')).toBeInTheDocument();
  expect(screen.queryByRole('region', { name: 'Decisions' })).toBeNull();
});

test('a decision on a part the page does not draw sits on the title', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec;
  spec.choices[0]!.pointer = '/components/schemas/Nowhere';
  render(<>{contracts.full!(sealed(JSON.stringify(spec)))}</>);
  const title = screen.getByRole('heading', { name: /Sleeper schedule endpoint/ });
  await userEvent.click(within(title).getByRole('button', { name: `Why: ${spec.choices[0]!.decision}` }));
  expect(screen.getByRole('dialog')).toBeInTheDocument();
});

test('a passed double-check is a green tick beside the title, on the card and the page', () => {
  const ok = { passed: true, gatesFailed: [], score: 1, checks: [] };
  render(<>{contracts.card!(sealed(raw(1), { verdict: ok }))}{contracts.full!(sealed(settings, { verdict: ok }))}{contracts.full!(sealed(raw(2), { verdict: { ...ok, passed: false } }))}</>);
  expect(screen.getAllByRole('img', { name: 'Double-checked' })).toHaveLength(2);
  expect(within(screen.getByRole('heading', { name: /Sleeper settings file/ })).getByRole('img', { name: 'Double-checked' })).toBeInTheDocument();
});

test('backticks in decisions and why notes render as code', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec;
  spec.choices[0]!.decision = 'Errors are `{ "error": string }`';
  render(<>{contracts.full!(sealed(JSON.stringify(spec)))}</>);
  await userEvent.click(screen.getAllByRole('button', { name: `Why: ${spec.choices[0]!.decision}` })[0]!);
  const panel = screen.getByRole('dialog');
  expect(within(panel).getByText('{ "error": string }').tagName).toBe('CODE');
  expect(panel).not.toHaveTextContent('`');
});

const settings = JSON.stringify(settingsSpec);

test('a json-schema card lists only the top-level fields', () => {
  render(<>{contracts.card!(sealed(settings))}</>);
  expect(screen.getByRole('heading', { name: 'Sleeper settings file' })).toBeInTheDocument();
  expect(screen.getByText('3 fields · version 1')).toBeInTheDocument();
  const list = within(screen.getByRole('list', { name: 'Fields' }));
  expect(list.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['leagueIdstringREQUIRED', 'modestring', 'notifyobject']);
  expect(screen.queryByText('notifyTime')).toBeNull();
  expect(screen.queryByRole('region', { name: 'Decisions' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Download JSON Schema file' })).toBeInTheDocument();
});

test('the json-schema full view shows each field with type, required, enum and example, and its info buttons open their choices', async () => {
  render(<>{contracts.full!(sealed(settings))}</>);
  const tree = within(screen.getByRole('region', { name: 'Fields' }));
  const id = tree.getByText('leagueId').closest('li') as HTMLElement;
  expect(id).toHaveTextContent('string');
  expect(id).toHaveTextContent('REQUIRED');
  expect(id).toHaveTextContent('example1234');
  expect(tree.getByText('mode').closest('li')).toHaveTextContent('one ofLINEUP · WAIVERS');
  expect(tree.getByText('notifyTime').closest('li')).toHaveTextContent('date-time');
  await userEvent.click(within(tree.getByText('mode').closest('li') as HTMLElement).getByRole('button', { name: 'Why: Mode is a closed set of two jobs.' }));
  expect(screen.getByRole('dialog', { name: 'Why: Mode is a closed set of two jobs.' })).toBeInTheDocument();
  // `/additionalProperties` is no field row, so it sits on the title.
  const title = screen.getByRole('heading', { name: /Sleeper settings file/ });
  expect(within(title).getByRole('button', { name: 'Why: The file is closed: unknown keys are rejected.' })).toBeInTheDocument();
});

const dts = '/** Core provides it. */\nexport interface Manifest {\n  name: string;\n}\n';
const tsSpec = JSON.stringify({ ...settingsSpec, kind: 'typescript', title: 'SDK surface', document: dts, choices: [{ id: 'm', pointer: '/Manifest', decision: 'Manifest is an interface.', why: [{ source: 'guide', text: 'Shape only.', ref: 'typescript#types' }] }] });

test('a typescript contract shows its source until its declarations load, its decision on the title meanwhile, then on its card', async () => {
  render(<>{contracts.full!(sealed(tsSpec))}</>);
  expect(screen.getByText('1 export · version 1')).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Declarations' })).getByText(/export interface Manifest/).tagName).toBe('PRE');
  const why = { name: 'Why: Manifest is an interface.' };
  expect(within(screen.getByRole('heading', { name: /SDK surface/ })).getByRole('button', why)).toBeInTheDocument();
  const card = await screen.findByRole('article', { name: 'Manifest' });
  expect(within(card).getByRole('button', why)).toBeInTheDocument();
  expect(within(screen.getByRole('heading', { name: /SDK surface/ })).queryByRole('button', why)).toBeNull();
  expect(screen.getByRole('button', { name: 'Download contract.d.ts' })).toBeInTheDocument();
});

test('a $ref parameter shows its name, type, default and place, and a choice on the shared parameter sits on its row', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec & { document: Record<string, any> };
  const params = {
    pageSize: { name: 'pageSize', in: 'query', description: 'Rows per page.', schema: { type: 'integer', default: 100 } },
    filter: { name: 'filter', in: 'query', required: true, schema: { $ref: '#/components/schemas/Filter' } },
  };
  spec.document.components = { ...spec.document.components, parameters: params, schemas: { ...spec.document.components?.schemas, Filter: { type: 'string', example: 'agent = "reviewer"' } } };
  const op = Object.values(spec.document.paths as Record<string, any>)[0].get;
  op.parameters = [{ $ref: '#/components/parameters/pageSize' }, { $ref: '#/components/parameters/filter' }];
  spec.choices = [{ id: 'f', pointer: '/components/parameters/filter', decision: 'Filter follows AIP-160.', why: [{ source: 'guide', text: 'One grammar.', ref: 'rest#filtering' }] }];
  render(<>{contracts.full!(sealed(JSON.stringify(spec)))}</>);
  const size = screen.getByText('pageSize').closest('li') as HTMLElement;
  expect(size).toHaveTextContent('integer');
  expect(size).toHaveTextContent('query');
  expect(size).toHaveTextContent('default100');
  expect(size).toHaveTextContent('Rows per page.');
  const filter = screen.getByText('filter').closest('li') as HTMLElement;
  expect(filter).toHaveTextContent('REQUIRED');
  expect(filter).toHaveTextContent('example');
  expect(within(filter).getByRole('button', { name: 'Why: Filter follows AIP-160.' })).toBeInTheDocument();
  expect(within(screen.getByRole('heading', { name: /Sleeper schedule endpoint/ })).queryByRole('button', { name: /^Why/ })).toBeNull();
});
