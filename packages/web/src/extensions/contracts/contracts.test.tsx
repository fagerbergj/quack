import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { artifactCard } from '../../Artifacts.tsx';
import { ArtifactPage } from '../../ArtifactPage.tsx';
import { delivered, versionOf } from '../../delivered.ts';
import type { SealedArtifact } from '../../api.ts';
import { contracts } from './index.ts';
import v1 from './fixtures/sleeper-schedule-v1.json';
import v2 from './fixtures/sleeper-schedule-v2.json';
import settingsSpec from './fixtures/sleeper-settings.json';
import { BeforeAfter } from './Sections.tsx';
import { fields, parseSpec, toYaml, type ContractSpec } from './spec.ts';

const raw = (v: number) => JSON.stringify(v === 1 ? v1 : v2);
// The seal stores the spec's own version as the row's outcome.
const outcomeOf = (content: string) => { try { return `v${(JSON.parse(content) as { version: number }).version}`; } catch { return undefined; } };
const sealed = (content: string, over: Partial<SealedArtifact> = {}): SealedArtifact => ({ kind: 'contract.spec', version: 1, hash: 'h', content, sealedAt: 1, verdict: null, outcome: outcomeOf(content), ...over });
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
  expect(screen.queryByText('Breaking changes')).toBeNull();
  expect(screen.queryByText(/^Changed from/)).toBeNull();
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

const changes = () => screen.getByRole('region', { name: /^Changes against/ });
const more = () => screen.getByRole('region', { name: 'More about this contract' });

test('on its own, version 2 leads with its changes one line each, then what could break and what was checked safe', async () => {
  show(2);
  expect(screen.getByRole('region', { name: 'What it does' })).toHaveTextContent("Gives the Sleeper page this league's upcoming runs and Sleeper's live week.");
  const list = changes();
  expect(within(list).getByRole('heading', { level: 2 })).toHaveTextContent('Changes against v1');
  const told = within(list).getAllByRole('listitem').slice(0, 3);
  expect(told.map((r) => r.textContent)).toEqual(['run-statusruns[].status: "done" | "next" More', 'run-orderRuns from the start of the week, oldest first More', 'result-linkDone runs link to their result More']);
  await userEvent.click(within(told[1]!).getByRole('button', { name: 'More' }));
  expect(told[1]).toHaveTextContent('Before Upcoming only, soonest firstWhy So the strip reads left to right through the week');
  expect(within(list).getByRole('heading', { name: 'What could break' })).toBeInTheDocument();
  expect(list).toHaveTextContent('The Sleeper page reads the first run as "next".');
  expect(list).toHaveTextContent('Callers: Sleeper page');
  expect(within(list).getByRole('heading', { name: 'Checked safe' }).nextElementSibling).toHaveTextContent('Still read-onlyDone runs carry where to open them');
});

test('the intent and the decisions are not on the page: each opens in a sheet, as stored', async () => {
  show(2);
  expect(screen.queryByText('Covers a week with no runs')).toBeNull();
  const rows = within(more()).getAllByRole('button').map((b) => b.textContent);
  expect(rows).toEqual(['Guarantees3', 'Out of scope1', 'Who it is for', 'Decisions7']);
  await userEvent.click(within(more()).getByRole('button', { name: /^Guarantees/ }));
  expect(within(screen.getByRole('dialog', { name: 'Guarantees' })).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['Read-only', 'Covers a week with no runs', 'Follows the REST guide']);
  await userEvent.keyboard('{Escape}');
  await userEvent.click(within(more()).getByRole('button', { name: /^Decisions/ }));
  expect(screen.getByRole('dialog', { name: 'Decisions' })).toHaveTextContent('Yes, always');
});

test('a spec with no breaks and no changes renders no changes section at all', () => {
  show(1);
  expect(screen.queryByRole('region', { name: /^Changes/ })).toBeNull();
  expect(more()).toBeInTheDocument();
});

test('a purpose too long for one sentence leads with its first one, the rest one press away', async () => {
  const purpose = 'One way to judge a sealed artifact version: the judge loads as its own extension through its own manifest, enabled per agent, and gates sealed versions through the gate chain. The rest follows.';
  render(<>{contracts.card!(sealed(JSON.stringify({ ...v1, intent: { ...v1.intent, purpose } })))}</>);
  const region = screen.getByRole('region', { name: 'What it does' });
  expect(region).toHaveTextContent(/^One way to judge a sealed artifact version\. Full purpose$/);
  await userEvent.click(within(region).getByRole('button', { name: 'Full purpose' }));
  expect(region).toHaveTextContent(`${purpose} Less`);
});

test('a long list of changes stays one line per change, and a was is shown as written', async () => {
  const many = Array.from({ length: 25 }, (_, i) => ({ choice: `c${i}`, was: 'GET / answered the whole list in one page', now: `Change ${i}`, why: 'Because' }));
  render(<>{contracts.card!(sealed(JSON.stringify({ ...v2, version: 3, changes: many })))}</>);
  expect(within(changes()).getByRole('heading', { level: 2 })).toHaveTextContent('Changes against v2');
  const rows = within(changes()).getAllByRole('listitem').slice(0, 25);
  expect(rows.map((r) => r.textContent)).toEqual(many.map((c) => `${c.choice}${c.now} More`));
  await userEvent.click(within(rows[0]!).getByRole('button', { name: 'More' }));
  expect(rows[0]).toHaveTextContent('Before GET / answered the whole list in one page');
});

test('the changes name their baseline and each break its migration, one press away', async () => {
  const breaking = [{ ...v2.compatibility.breaking[0], migration: 'Read the run marked next' }];
  render(<>{contracts.card!(sealed(JSON.stringify({ ...v2, baseline: 'shipped', compatibility: { ...v2.compatibility, breaking } })))}</>);
  expect(within(changes()).getByRole('heading', { level: 2 })).toHaveTextContent('Changes against the shipped code');
  expect(changes()).not.toHaveTextContent('Read the run marked next');
  await userEvent.click(within(changes()).getAllByRole('button', { name: 'More' }).at(-1)!);
  expect(changes()).toHaveTextContent('Migration Read the run marked next');
});

// Real stored shapes: `change` for what, callers in one string, no migration.
test('a break sealed before the migration rule still draws', () => {
  const breaking = [{ change: 'agent.json is closed', callers: 'The 13 bundles under agents/*/agent.json', migration: 'Delete unknown keys' }, { choice: 'errors', was: 'a', now: 'b', callers: ['repo/packages/web/src/api.ts:28'] }];
  render(<>{contracts.card!(sealed(JSON.stringify({ ...v2, baseline: 4, compatibility: { breaking, safe: [] } })))}</>);
  expect(within(changes()).getByRole('heading', { level: 2 })).toHaveTextContent('Changes against v4');
  expect(changes()).toHaveTextContent('agent.json is closed');
  expect(changes()).toHaveTextContent('Callers: The 13 bundles under agents/*/agent.json');
  expect(changes()).toHaveTextContent('Callers: repo/packages/web/src/api.ts:28');
});

test('a text diff shows a short value on one line and prose as before and after blocks, each tinting only its own words', () => {
  const { container, unmount } = render(<BeforeAfter was="number" now="string" words />);
  expect(container).toHaveTextContent('Removed: number→Added: string');
  unmount();
  render(<BeforeAfter was="The list answers every registration in one page, soonest first, with no paging at all." now="The list answers registrations a page at a time, soonest first, with nextPageToken for the rest." words />);
  const side = (t: string) => screen.getByText(t).parentElement!;
  expect([side('Before').querySelectorAll('del').length > 0, side('Before').querySelector('ins')]).toEqual([true, null]);
  expect([side('After').querySelectorAll('ins').length > 0, side('After').querySelector('del')]).toEqual([true, null]);
  expect(side('After')).toHaveTextContent('nextPageToken');
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

test('an answer shows the question as asked and the user\'s answer', async () => {
  show(1);
  await userEvent.click(screen.getAllByRole('button', { name: 'Why: GET with no body' })[0]!);
  const panel = screen.getByRole('dialog', { name: 'Why: GET with no body' });
  expect(within(panel).getByText('Answered “Should this ever start a job?”')).toBeInTheDocument();
  expect(within(panel).getByText('No, it only reads')).toBeInTheDocument();
});

// The artifact page lists a kind's versions without content, then reads the shown one alone.
const pageFetch = (list: SealedArtifact[]) => vi.fn(async (url: string) => {
  const n = /version:(\d+)/.exec(decodeURIComponent(url))?.[1];
  const rows = list.map((a) => ({ session: 'home:main', kind: a.kind, version: a.version, hash: a.hash, sealedAt: new Date(a.sealedAt).toISOString(), ...(n && { content: a.content }) }));
  return new Response(JSON.stringify({ artifacts: n ? rows.filter((r) => r.version === Number(n)) : rows }));
});

test('the page links to its session and offers versions, reading only the shown version', async () => {
  const list = [sealed(raw(1), { hash: 'h1' }), sealed(raw(2), { version: 2, hash: 'h2' })];
  const fetcher = pageFetch(list);
  vi.stubGlobal('fetch', fetcher);
  location.hash = '#/artifacts';
  render(<ArtifactPage sessionKey="home:main" kind="contract.spec" version={1} rows={undefined} />);
  expect(await screen.findByText('1 route · version 1')).toBeInTheDocument();
  expect(fetcher.mock.calls.map(([u]) => decodeURIComponent(u)).filter((u) => u.includes('version:'))).toHaveLength(1);
  expect(screen.getByRole('link', { name: 'Open its session' })).toHaveAttribute('href', '#/s/home%3Amain');
  await userEvent.click(screen.getByRole('button', { name: 'Version: Version 1' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Version 2' }));
  expect(location.hash).toBe('#/s/home%3Amain/a/contract.spec?v=2');
  vi.unstubAllGlobals();
});

test('a spec version sealed more than once groups by the row outcome and shows the delivered seal', () => {
  const ok = { passed: true, gatesFailed: [], score: 1, checks: [] };
  const draft = JSON.stringify({ ...JSON.parse(raw(2)), title: 'Rejected draft' });
  const list = [sealed(raw(1), { version: 1, verdict: ok }), sealed(draft, { version: 2, verdict: { ...ok, passed: false } }), sealed(raw(2), { version: 3, verdict: ok })];
  expect(delivered(list).map((a) => [a.version, versionOf(a)])).toEqual([[3, 2], [1, 1]]);
  expect(delivered(list.slice(0, 2)).map((a) => a.version)).toEqual([2, 1]);
  const released = [sealed(raw(2), { version: 2, verdict: { ...ok, passed: false, limitReached: true } }), sealed(draft, { version: 3, verdict: { ...ok, passed: false } })];
  expect(delivered(released).map((a) => a.version)).toEqual([2]);
});

test('a version is labelled from its outcome without reading content, and from its sealed number when it has none', () => {
  const { content: _, ...older } = sealed(raw(2), { version: 7, outcome: 'v3 typescript' });
  expect(versionOf(older)).toBe(3);
  expect(versionOf({ ...older, outcome: 'v12' })).toBe(12);
  expect(versionOf({ ...older, outcome: undefined })).toBe(7);
  expect(versionOf({ ...older, outcome: 'approve', kind: 'github.review' })).toBe(7);
  expect(contracts.card!(older)).toBeUndefined();
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

test('the view keeps the operation detail, with no decisions list beside it', () => {
  show(1);
  expect(screen.getByText('Responses')).toBeInTheDocument();
  expect(screen.queryByRole('region', { name: 'Decisions' })).toBeNull();
});

test('a decision on a part the page does not draw sits on the title', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec;
  spec.choices[0]!.pointer = '/components/schemas/Nowhere';
  render(<>{contracts.card!(sealed(JSON.stringify(spec)))}</>);
  const title = screen.getByRole('heading', { name: /Sleeper schedule endpoint/ });
  await userEvent.click(within(title).getByRole('button', { name: `Why: ${spec.choices[0]!.decision}` }));
  expect(screen.getByRole('dialog')).toBeInTheDocument();
});

test('a passed double-check is a green tick beside the title', () => {
  const ok = { passed: true, gatesFailed: [], score: 1, checks: [] };
  render(<>{contracts.card!(sealed(raw(1), { verdict: ok }))}{contracts.card!(sealed(settings, { verdict: ok }))}{contracts.card!(sealed(raw(2), { verdict: { ...ok, passed: false } }))}</>);
  expect(screen.getAllByRole('img', { name: 'Double-checked' })).toHaveLength(2);
  expect(within(screen.getByRole('heading', { name: /Sleeper settings file/ })).getByRole('img', { name: 'Double-checked' })).toBeInTheDocument();
});

test('backticks in decisions and why notes render as code', async () => {
  const spec = JSON.parse(raw(1)) as ContractSpec;
  spec.choices[0]!.decision = 'Errors are `{ "error": string }`';
  render(<>{contracts.card!(sealed(JSON.stringify(spec)))}</>);
  await userEvent.click(screen.getAllByRole('button', { name: `Why: ${spec.choices[0]!.decision}` })[0]!);
  const panel = screen.getByRole('dialog');
  expect(within(panel).getByText('{ "error": string }').tagName).toBe('CODE');
  expect(panel).not.toHaveTextContent('`');
});

const settings = JSON.stringify(settingsSpec);

test('the json-schema view shows each field with type, required, enum and example, and its info buttons open their choices', async () => {
  render(<>{contracts.card!(sealed(settings))}</>);
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
  render(<>{contracts.card!(sealed(tsSpec))}</>);
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
  render(<>{contracts.card!(sealed(JSON.stringify(spec)))}</>);
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
