import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { classDiagram } from './ClassDiagram.tsx';
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

test('the class diagram draws the types only, with an arrow from each type to the types it uses', () => {
  const dg = classDiagram(declarations(dts).decls);
  expect(dg.boxes.map((b) => b.decl.name).sort()).toEqual(['Limits', 'Manifest', 'Running', 'Store']);
  expect(dg.edges.map((e) => `${e.from}>${e.to}`).sort()).toEqual(['Manifest>Limits', 'Manifest>Store', 'Running>Manifest']);
  expect(dg.boxes.find((b) => b.decl.name === 'Store')!.lines).toEqual([["| kind: 'sqlite'", ''], ["| kind: 'postgres'", '']]);
  for (const b of dg.boxes) expect(b.x >= 0 && b.y >= 0 && b.x + b.w <= dg.width && b.y + b.h <= dg.height).toBe(true);
});

test('a box with many fields shows the first eight and counts the rest', () => {
  const wide = `export type Wide = { ${Array.from({ length: 11 }, (_, i) => `f${i}: string;`).join(' ')} };`;
  const [box] = classDiagram(declarations(wide).decls).boxes;
  expect(box!.lines).toHaveLength(9);
  expect(box!.lines[8]).toEqual(['+ 3 more', '']);
});

test('a TypeScript contract opens as a diagram and a card per declaration, with linked types and the why on its card', async () => {
  const { contracts } = await import('./index.ts');
  const spec = { kind: 'typescript', title: 'Stores', version: 1, intent: { purpose: 'p', nonGoals: [], audience: 'a', consumers: [], guarantees: [] }, document: dts, choices: [{ id: 's', pointer: '/Store', decision: 'Store is a union.', why: [{ source: 'guide', text: 'One shape per kind.', ref: 'typescript#unions' }] }] };
  render(<>{contracts.full!({ kind: 'contract.spec', version: 1, hash: 'h', content: JSON.stringify(spec), sealedAt: 1, verdict: null })}</>);
  const diagram = await screen.findByRole('region', { name: 'Class diagram' });
  expect(within(diagram).getByRole('button', { name: 'Manifest, go to its declaration' })).toBeInTheDocument();
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
