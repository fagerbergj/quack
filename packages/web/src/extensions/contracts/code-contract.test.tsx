import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { defaultFocus, focusView, HUB } from './class-model.ts';
import { declarations } from './typescript-model.ts';

const dts = `import type { Context } from '@earendil-works/chord/context';

/** Where a store lives. */
export type Store =
  | { kind: 'sqlite' }
  | { kind: 'postgres'; /** The variable holding the URL. */ urlEnv: string };

type Limits = { max?: number };

/** One extension. */
export type Manifest = {
  /** Its name. */
  name: string;
  store?: Store;
  limits: { max: number; each?: Limits[] };
  close(): Promise<void>;
};

export type Running = Manifest & { since: number };

/**
 * Starts one.
 * @param m The manifest to start.
 */
export declare function start(m: Manifest, ctx?: Context): { stop: () => void };

export declare const version: string;
`;

const byName = (name: string) => declarations(dts).decls.find((d) => d.name === name)!;
const rows = (name: string) => byName(name).rows.map((r) => `${'  '.repeat(r.depth)}${r.name}: ${r.type}${r.required ? ' *' : ''}`);

test('a type lists its fields with required, docs and inline objects nested under their field', () => {
  expect(rows('Manifest')).toEqual(['name: string *', 'store: Store', 'limits: object *', '  max: number *', '  each: Limits[]', 'close: () => Promise<void> *']);
  expect(byName('Manifest').rows[0]!.desc).toBe('Its name.');
  expect(byName('Manifest').doc).toBe('One extension.');
  expect(byName('Manifest').uses).toEqual(['Store', 'Limits']);
});

test('a union of objects is one row per variant, named by its literal field', () => {
  expect(rows('Store')).toEqual(['kind: \'sqlite\': object', '  kind: \'sqlite\' *', 'kind: \'postgres\': object', '  kind: \'postgres\' *', '  urlEnv: string *']);
  expect(byName('Store').rows[0]!.where).toBe('one of');
  expect(byName('Store').rows[4]!.desc).toBe('The variable holding the URL.');
});

test('an intersection lists its own fields beside the types it includes', () => {
  expect(rows('Running')).toEqual(['since: number *']);
  expect(byName('Running').text).toBe('Manifest & …');
  expect(byName('Running').uses).toEqual(['Manifest']);
});

test('a function lists its parameters with their @param text, then what it returns', () => {
  expect(rows('start')).toEqual(['m: Manifest *', 'ctx: Context', 'returns: object', '  stop: () => void *']);
  expect(byName('start').rows[0]!.desc).toBe('The manifest to start.');
});

test('constants, unexported types and imports are kept apart', () => {
  const m = declarations(dts);
  expect(m.decls.map((d) => `${d.kind} ${d.name}${d.exported ? '' : ' (local)'}`)).toEqual(['type Store', 'type Limits (local)', 'type Manifest', 'type Running', 'function start', 'const version']);
  expect(byName('version').text).toBe('string');
  expect(m.imports).toEqual([{ from: '@earendil-works/chord/context', names: ['Context'] }]);
});

const edgesOf = (v: ReturnType<typeof focusView>) => v.edges.map((e) => `${e.source}.${v.boxes.find((b) => b.decl.name === e.source)!.rows[e.row]!.name}>${e.target}`).sort();

test('the diagram centres on one type: it, what it uses and what uses it, each arrow leaving from the field that names the type', () => {
  const v = focusView(declarations(dts).decls, 'Manifest');
  expect(v.boxes.map((b) => `${b.decl.name}${b.focus ? '*' : ''}`).sort()).toEqual(['Manifest*', 'Running', 'Store']);
  expect(edgesOf(v)).toEqual(['Manifest.store?:>Store', 'Running.extends>Manifest']);
  expect(v.boxes.find((b) => b.decl.name === 'Store')!.rows.map((r) => `${r.name} ${r.type}`)).toEqual(["| kind: 'sqlite' ", "| kind: 'postgres' "]);
  // Limits is named only inside an inline object, two hops from the focus, so it is not drawn.
  expect(focusView(declarations(dts).decls, 'Store').boxes.map((b) => b.decl.name).sort()).toEqual(['Manifest', 'Store']);
  expect(focusView(declarations(dts).decls, 'start').boxes).toEqual([]);
});

test('the diagram opens on the root type that names the most others, else the most-named exported type', () => {
  expect(defaultFocus(declarations(dts).decls)).toBe('Running');
  const flat = 'export type A = { b: B }; export type B = { n: number }; export type C = { b: B };';
  expect(defaultFocus(declarations(flat).decls)).toBe('A');
  expect(defaultFocus(declarations('export type A = { n: number }; export type B = A; type C = { a: A };').decls)).toBe('B');
});

test(`a type more than ${HUB} boxes in view name is a chip on each field, not an arrow from each; one out of view is a chip too`, () => {
  const users = Array.from({ length: HUB + 1 }, (_, i) => `export type U${i} = { id: Id; root: Root };`).join('\n');
  const src = `export type Id = string;\nexport type Root = { ${Array.from({ length: HUB + 1 }, (_, i) => `u${i}: U${i};`).join(' ')} id: Id };\n${users}\nexport type Far = { u: U0 };`;
  const v = focusView(declarations(src).decls, 'Root');
  expect(v.boxes.map((b) => b.decl.name)).not.toContain('Id');
  expect(v.boxes.find((b) => b.focus)!.rows.at(-1)).toEqual({ name: 'id:', type: 'Id', edges: [], chips: ['Id'] });
  const u0 = v.boxes.find((b) => b.decl.name === 'U0')!;
  expect(u0.rows.map((r) => [r.name, r.edges, r.chips])).toEqual([['id:', [], ['Id']], ['root:', ['Root'], []]]);
  const hubbed = focusView(declarations(src).decls, 'U0');
  expect(hubbed.boxes.map((b) => b.decl.name).sort()).toEqual(['Far', 'Id', 'Root', 'U0']);
  expect(edgesOf(hubbed)).toEqual(['Far.u:>U0', 'Root.id:>Id', 'Root.u0:>U0', 'U0.id:>Id', 'U0.root:>Root']);
  expect(hubbed.boxes.find((b) => b.decl.name === 'Root')!.rows[1]).toEqual({ name: 'u1:', type: 'U1', edges: [], chips: ['U1'] });
});

test('a TypeScript contract opens as a diagram and a card per declaration, with linked types and the why on its card', async () => {
  const { contracts } = await import('./index.ts');
  const spec = { kind: 'typescript', title: 'Stores', version: 1, intent: { purpose: 'p', nonGoals: [], audience: 'a', consumers: [], guarantees: [] }, document: dts, choices: [{ id: 's', pointer: '/Store', decision: 'Store is a union.', why: [{ source: 'guide', text: 'One shape per kind.', ref: 'typescript#unions' }] }] };
  render(<>{contracts.card!({ kind: 'contract.spec', version: 1, hash: 'h', content: JSON.stringify(spec), sealedAt: 1, verdict: null })}</>);
  const diagram = await screen.findByRole('region', { name: 'Class diagram' });
  // A click alone: d3-zoom's mousedown reads the event's view, which jsdom leaves null.
  fireEvent.click(await within(diagram).findByRole('button', { name: 'Centre the diagram on Manifest' }));
  expect(await within(diagram).findByRole('button', { name: 'Manifest, go to its declaration' })).toBeInTheDocument();
  expect(within(diagram).getByRole('heading')).toHaveTextContent('around Manifest');
  const list = screen.getByRole('region', { name: 'Declarations' });
  expect(within(list).getByRole('heading', { name: 'Functions 1' })).toBeInTheDocument();
  const store = within(list).getByRole('article', { name: 'Store' });
  await userEvent.click(within(store).getByRole('button', { name: 'Why: Store is a union.' }));
  expect(within(screen.getByRole('dialog', { name: 'Why: Store is a union.' })).getByText('One shape per kind.')).toBeInTheDocument();
  const manifest = within(list).getByRole('article', { name: 'Manifest' });
  await userEvent.click(within(manifest).getAllByRole('button', { name: 'Store' })[0]!);
  expect(store).toHaveFocus();
  expect(within(manifest).getByText(/Used by/)).toHaveTextContent('Used by Running, start');
});

test('a doc runs until its first @tag; a line starting with a bare @ is prose', () => {
  const [d] = declarations('/**\n * Pings the channel.\n * @ mentions go to ops.\n * @deprecated Use pingAll.\n */\nexport declare function ping(): void;').decls;
  expect(d!.doc).toBe('Pings the channel.\n@ mentions go to ops.');
});

test('a source the parser rejects keeps the view on its plain source, with no diagram or cards', async () => {
  // Not valid TypeScript: tsc answers TS1005 ':' expected, so such a contract cannot pass its compile check.
  const src = 'export interface K { () => void }';
  expect(() => declarations(src)).toThrow();
  const { contracts } = await import('./index.ts');
  const spec = { kind: 'typescript', title: 'Broken', version: 1, intent: { purpose: 'p', nonGoals: [], audience: 'a', consumers: [], guarantees: [] }, document: src, choices: [] };
  render(<>{contracts.card!({ kind: 'contract.spec', version: 1, hash: 'h', content: JSON.stringify(spec), sealedAt: 1, verdict: null })}</>);
  await act(async () => { await import('./typescript-model.ts'); });
  expect(within(screen.getByRole('region', { name: 'Declarations' })).getByText(src).tagName).toBe('PRE');
  expect(screen.queryByRole('region', { name: 'Class diagram' })).toBeNull();
  expect(screen.queryByRole('article')).toBeNull();
});

test('an enum lists its members with their values, implicit ones numbered as the compiler does, and its box uses =', () => {
  const [e] = declarations("/** Levels. */\nexport declare enum Level { /** Quiet. */ Info = 'info', Error = 'error' }").decls;
  expect(e).toMatchObject({ name: 'Level', kind: 'enum', doc: 'Levels.' });
  expect(e!.rows.map((r) => [r.name, r.type, r.desc])).toEqual([['Info', "'info'", 'Quiet.'], ['Error', "'error'", undefined]]);
  const [mode] = declarations('export enum Mode { Auto, Fast = 5, Faster, Named = "n" }').decls;
  expect(mode!.rows.map((r) => [r.name, r.type])).toEqual([['Auto', '0'], ['Fast', '5'], ['Faster', '6'], ['Named', '"n"']]);
  expect(focusView([mode!], 'Mode').boxes[0]!.rows.map((r) => `${r.name} ${r.type}`)).toEqual(['Auto = 0', 'Fast = 5', 'Faster = 6', 'Named = "n"']);
  const [neg] = declarations('export enum E { A = -1, B, C = 0x10, D }').decls;
  expect(neg!.rows.map((r) => [r.name, r.type])).toEqual([['A', '-1'], ['B', '0'], ['C', '0x10'], ['D', '17']]);
});

test('tabbing to an info button shows its reasons, and tabbing away hides them', async () => {
  const { Info } = await import('./Info.tsx');
  const c = { id: 'a', pointer: '/x', decision: 'X is a string.', why: [{ source: 'guide' as const, text: 'Plain.', ref: 'g' }] };
  render(<><Info choices={[c]} /><button type="button">next</button></>);
  await userEvent.tab();
  expect(screen.getByRole('dialog', { name: 'Why: X is a string.' })).toHaveTextContent('Plain.');
  await userEvent.tab();
  expect(screen.queryByRole('dialog')).toBeNull();
  await userEvent.click(screen.getByRole('button', { name: 'Why: X is a string.' }));
  expect(screen.getByRole('dialog')).toBeInTheDocument();
});

test('a press on the focused button does not swallow the next keyboard focus', async () => {
  const { Info } = await import('./Info.tsx');
  const c = { id: 'a', pointer: '/x', decision: 'X is a string.', why: [{ source: 'guide' as const, text: 'Plain.', ref: 'g' }] };
  render(<><Info choices={[c]} /><button type="button">next</button></>);
  const info = screen.getByRole('button', { name: 'Why: X is a string.' });
  await userEvent.tab();
  await userEvent.click(info);
  await userEvent.click(info);
  expect(screen.queryByRole('dialog')).toBeNull();
  await userEvent.tab();
  await userEvent.tab({ shift: true });
  expect(info).toHaveFocus();
  expect(screen.getByRole('dialog', { name: 'Why: X is a string.' })).toBeInTheDocument();
});
