import { cp, mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { contractDeclaration, contractsSessionExtensions, type ContractSpec } from '@quack/contracts';
import { sessionDirName } from '@quack/distributor';
import { Approvals, resolveApproval } from '@quack/sdk';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { scheduleSpecV1, scheduleSpecV2, SLEEPER_PAGE } from '../../packages/contracts/test/fixtures.ts';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));

type Msgs = { messages: Array<{ content: unknown }> };
const last = (c: Msgs) => { const m = c.messages.at(-1)!.content; return typeof m === 'string' ? m : (m as Array<{ text?: string }>).map((p) => p.text ?? '').join(''); };
const toolCall = (name: string, args: object) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const writeSpec = (s: ContractSpec) => toolCall('write', { path: 'artifacts/spec.json', content: JSON.stringify(s, null, 2) });

/** The extension SDK's manifest surface, cut to a self-contained declaration file: many types, a few used everywhere. */
export const SDK_V1 = `/** A JSON value. */
export type Json = string | number | boolean | null | Json[] | { [key: string]: Json };
/** An HTTP request as an app receives it. */
export interface Request { method: string; url: string; headers: Record<string, string> }
/** The response an app writes. */
export interface Response { status(code: number): Response; json(body: Json): void }
/** A route a credential-free request may reach. */
export type PageRoute = { method: string; path: string };
/** One sealed version of an artifact. */
export type SealedArtifact = { kind: string; version: number; hash: string; content: string; verdict: Json | null };
/** Where an extension keeps its data. */
export type StoreChoice = { kind: 'sqlite'; path: string } | { kind: 'postgres'; url: string };
/** One extension's entry in the deployment file. */
export type ExtensionEntry = { settings: Json; store?: StoreChoice };
/** One model a provider serves. */
export type ModelEntry = { contextWindow: number; images?: boolean };
/** A model provider. */
export type ProviderEntry = { baseUrl: string; models: Record<string, ModelEntry> };
/** The deployment file. */
export type DeploymentFile = { extensions: Record<string, ExtensionEntry>; providers: Record<string, ProviderEntry> };
/** What an action's start is given. */
export type ActionContext = { distributorUrl: string; env: Record<string, string | undefined>; deployment: DeploymentFile };
/** Work an extension can start on a person's behalf. */
export interface Action { name: string; summary: string; params: Json; start(params: Json, ctx: ActionContext): Promise<{ session: string; title: string }> }
/** What the distributor gives an app. */
export type AppContext = { settings: Json; dbPath: string; distributorUrl: string; deployment: DeploymentFile; env: Record<string, string | undefined> };
/** An extension's in-process half. */
export interface App { handle(req: Request, res: Response): Promise<void>; pages?: PageRoute[] }
/** What a recorded session holds. */
export type RecordedSession = { docs: Record<string, Json>; artifacts: Record<string, SealedArtifact[]> };
/** A replay's stand-in for an extension's service. */
export type EvalService = { env: Record<string, string>; captured: Json[]; close(): Promise<void> };
/** How eval replays and scores one artifact kind. */
export interface EvalAdapter { kind: string; services?: Record<string, (pin: Json) => Promise<EvalService>>; judge?: { rubric: Json; evidence(session: RecordedSession, content: string): Promise<Json> } }
/** A workflow an extension contributes. */
export type ContributedWorkflow = { build(config: Json): Json; env: string[] };
/** What an extension package exports as its manifest. */
export interface ExtensionManifest { name: string; app?(ctx: AppContext): App | Promise<App>; pages?: PageRoute[]; actions?: Action[]; workflows?: Record<string, ContributedWorkflow>; evalAdapters?: EvalAdapter[]; validate?(config: Json, deployment: DeploymentFile): string[] }
`;

const SDK_V2 = SDK_V1
  .replace('export type ModelEntry = { contextWindow: number; images?: boolean };', 'export type ModelEntry = { contextWindow: number; maxTokens?: number; images?: boolean };')
  .replace('export interface App { handle(req: Request, res: Response): Promise<void>; pages?: PageRoute[] }', 'export interface App { handle(req: Request, res: Response): Promise<void>; stop?(): Promise<void>; pages?: PageRoute[] }');

const why = [{ source: 'answer' as const, text: 'The request describes the manifest surface.', ref: 'request' }];
const sdkSpec = (version: number, document: string): ContractSpec => ({
  kind: 'typescript',
  title: 'Extension SDK manifest',
  version,
  intent: { purpose: 'The types an extension package exports and the distributor reads.', nonGoals: ['Runtime behaviour'], audience: 'internal', consumers: ['Extensions', 'The distributor'], guarantees: ['Additive changes only within a major version'] },
  document,
  choices: [
    { id: 'manifest', pointer: '/ExtensionManifest', decision: 'One manifest object per package names everything it contributes.', why },
    { id: 'json', pointer: '/Json', decision: 'Settings and params are plain JSON.', why },
    ...(version > 1 ? [{ id: 'stop', pointer: '/App', decision: 'An app may stop its background loops.', why }] : []),
  ],
  ...(version > 1 && {
    changes: [{ choice: 'stop', was: 'Apps had no stop hook', now: 'App has an optional stop()', why: 'The distributor drains apps on shutdown.' }],
    compatibility: { breaking: [], safe: ['stop and maxTokens are optional, so existing extensions still compile'] },
  }),
});

type Contract = { key: string; title: string; asks: string[]; versions: ContractSpec[]; files?: string[] };

const CONTRACTS: Contract[] = [
  { key: 'contracts:qa-sdk', title: 'Extension SDK manifest', asks: ['Define the extension SDK manifest types', 'Add an optional stop hook to App and maxTokens to models'], versions: [sdkSpec(1, SDK_V1), sdkSpec(2, SDK_V2)] },
  {
    key: 'contracts:qa-schedule', title: 'Sleeper schedule endpoint', asks: ['Define GET /apps/sleeper/schedule for the Sleeper page', 'Make the run time Unix seconds'],
    versions: [scheduleSpecV1({ versioning: 'request', time: 'request' }), scheduleSpecV2({ versioning: 'request', time: 'request' }, [SLEEPER_PAGE])],
    files: ['packages/sleeper/src/service.ts', 'packages/web/src/extensions/sleeper/api.ts'],
  },
];

// A check whose aggregate names its passing answers (a pitfall passes on "no") gets that answer; every other judge question gets "yes".
const PASSING = Object.values(contractDeclaration.checks).flatMap((c) => ('pass' in c.aggregate && c.aggregate.pass ? [[c.question.instructions, c.aggregate.pass[0]!] as const] : []));
const answer = (asked: string) => PASSING.find(([q]) => asked.includes(q))?.[1] ?? 'yes';

// Judge questions get the passing answer; every other turn writes the next version and submits it.
function scripted(versions: ContractSpec[]) {
  const faux = fauxProvider({ models: [{ id: 'seed' }] });
  const queue = versions.flatMap((v) => [() => writeSpec(v), () => toolCall('submit_contract', {}), () => fauxAssistantMessage(`Version ${v.version} is approved.`)]);
  faux.setResponses(Array.from({ length: 200 }, () => (c: Msgs) => (last(c).startsWith('Decision:') ? fauxAssistantMessage(JSON.stringify({ choice: answer(last(c)), confidence: 1, reason: 'fine' })) : queue.shift()!())));
  return faux;
}

async function nextApproval(s: Awaited<ReturnType<typeof openSession>>): Promise<string> {
  await s.root.commit(async (tx) => { await tx.doc(Approvals, s.root.id); }, ctx);
  const watch = (await s.harness.watchDoc(Approvals, s.root.id, ctx))!;
  const open = () => Object.entries(watch.value?.items ?? {}).find(([, a]) => !a.verdict)?.[0];
  return new Promise<string>((resolve) => {
    if (open()) return resolve(open()!);
    watch.start(async () => { if (open()) resolve(open()!); });
  }).finally(() => watch.stop());
}

/** A contract session as the distributor keeps one: version 1 approved, version 2 sealed, passed and waiting for approval. */
async function seedOne(dataDir: string, c: Contract) {
  const dir = join(dataDir, sessionDirName(c.key));
  await mkdir(dir, { recursive: true });
  const now = String(Date.now());
  await Promise.all([['session-key', c.key], ['agent', 'contracts'], ['title', c.title], ['created', now], ['activity', now]].map(([f, v]) => writeFile(join(dir, f), v)));
  await cp(join(ROOT, 'agents/contracts'), join(dir, 'agent-def'), { recursive: true });
  const faux = scripted(c.versions);
  const models = buildModels(parseDeployment({ defaultModel: 'unused/m', providers: { unused: { baseUrl: 'http://127.0.0.1:9/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } }));
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...contractsSessionExtensions({})] } });
  for (const f of c.files ?? []) {
    await mkdir(dirname(join(s.workspace, f)), { recursive: true });
    await cp(join(ROOT, f), join(s.workspace, f));
  }
  const first = s.submit(c.asks[0]!, 'seed-1', true);
  await resolveApproval(s.root, await nextApproval(s), { decision: 'approve', by: { kind: 'person', name: 'qa' } }, ctx);
  await first;
  void s.submit(c.asks[1]!, 'seed-2', false);
  await nextApproval(s);
  await s.close();
}

/** Writes the QA contract sessions into a distributor data dir before it starts. */
export async function seedContracts(dataDir: string) {
  for (const c of CONTRACTS) await seedOne(dataDir, c);
}
