import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { createModels, createProvider, envApiKeyAuth, type MutableModels } from '@earendil-works/pi-ai';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import type { Extension } from '@earendil-works/pi-durable';
import type { ExtensionEntry, StoreEntry } from '@quack/sdk';
import type { ExecPolicy } from './exec.ts';
import type { AgentSpec } from './runtime.ts';

/** `images` declares image input; without it pi-ai replaces image blocks with a text placeholder before the request. A model with no `contextWindow` is listed but not selectable. */
export type ModelLimits = { contextWindow?: number; maxTokens?: number; reasoning?: boolean; images?: boolean; /** A notional price per million tokens (power, say); omitted parts are free. pi-ai turns it into each call's `usage.cost`. */ price?: Partial<Price> };
type Price = { input: number; output: number; cacheRead: number; cacheWrite: number };
const PRICE_KEYS = ['input', 'output', 'cacheRead', 'cacheWrite'];
/** `systemone` providers are decision models such as Clef: classifiers with an input cap and no generation. */
type ProviderConfig = { baseUrl: string; api?: 'openai-completions' | 'systemone'; apiKeyEnv?: string; /** How many slots the slots gate hands this provider out. */ slots?: number; /** Hand-written entries; `resolveDeployment` merges them over what `GET {baseUrl}/models` states. */ models?: Record<string, ModelLimits> };

/** Per deployment: the OpenAI-compatible providers sessions may use. */
export type DeploymentConfig = { providers: Record<string, ProviderConfig>; /** The provider/model of agents that name none. */ defaultModel: string; timeZone?: string; /** Named stores consumers reference by name. */ stores?: Record<string, StoreEntry>; /** Extensions mounted in-process under /apps/<name>; the shape of each one's `settings` is the extension's own. */ extensions?: Record<string, ExtensionEntry>; /** How agent shells are confined; see ExecPolicy. */ exec?: ExecPolicy; /** Per-session limits on the subagent tool every session has (see @quack/subagent). */ subagents?: { maxConcurrent?: number; maxTotal?: number } };

/** Per agent: its model as provider/model (parseDefinition fills the deployment's defaultModel when it names none), a prompt file beside it, and the extensions it selects by name with their config. */
export type AgentDefinition = { name: string; version: number; model: string; prompt?: string; extensions: Record<string, unknown>; /** The longest one shell command may run for this agent. */ exec?: { timeoutSeconds?: number }; /** Model responses one run may take before it is failed; absent takes the default ceiling. */ turnCeiling?: number; /** How long an idle session keeps its log and workspace; absent keeps it forever. A retention sweep enforces it. */ retention?: { idleDays: number }; /** `false` leaves the session without the subagent tool. */ subagents?: false };

/** An entry turns an agent's config into installed extensions; `env` names what it reads from the session process, which only agents selecting it receive. */
export type CatalogEntry = ((config: unknown) => Extension[] | Promise<Extension[]>) & { env?: readonly string[] | ((config: unknown) => readonly string[]); validate?: (config: unknown, deployment: DeploymentConfig) => string[]; requires?: readonly string[]; optional?: readonly string[] };
/** Extensions compiled into the runtime image, by name. */
export type Catalog = Record<string, CatalogEntry>;

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const positive = (v: unknown) => typeof v === 'number' && Number.isInteger(v) && v > 0;

function providerProblems(id: string, p: unknown): string[] {
  if (!isObject(p)) return [`providers.${id} must be an object`];
  const problems: string[] = [];
  if (typeof p.baseUrl !== 'string' || !/^https?:\/\//.test(p.baseUrl)) problems.push(`providers.${id}.baseUrl must be an http(s) URL`);
  if (p.apiKeyEnv !== undefined && typeof p.apiKeyEnv !== 'string') problems.push(`providers.${id}.apiKeyEnv must be a string`);
  if (p.api !== undefined && p.api !== 'openai-completions' && p.api !== 'systemone') problems.push(`providers.${id}.api must be openai-completions or systemone`);
  if (p.slots !== undefined && !positive(p.slots)) problems.push(`providers.${id}.slots must be a positive integer`);
  if (p.models === undefined) return problems;
  if (!isObject(p.models)) return [...problems, `providers.${id}.models must be an object`];
  return [...problems, ...Object.entries(p.models).flatMap(([m, l]) => limitProblems(`providers.${id}.models.${m}`, l))];
}

function limitProblems(at: string, l: unknown): string[] {
  if (!isObject(l)) return [`${at} must be an object`];
  const problems = ['contextWindow', 'maxTokens'].filter((k) => l[k] !== undefined && !positive(l[k])).map((k) => `${at}.${k} must be a positive integer`);
  problems.push(...['reasoning', 'images'].filter((k) => l[k] !== undefined && typeof l[k] !== 'boolean').map((k) => `${at}.${k} must be a boolean`));
  return [...problems, ...priceProblems(`${at}.price`, l.price)];
}

function priceProblems(at: string, p: unknown): string[] {
  if (p === undefined) return [];
  const ok = isObject(p) && Object.entries(p).every(([k, v]) => PRICE_KEYS.includes(k) && typeof v === 'number' && Number.isFinite(v) && v >= 0);
  return ok ? [] : [`${at} must map ${PRICE_KEYS.join(', ')} to non-negative numbers (per million tokens)`];
}

function execProblems(at: string, e: unknown, deployment: boolean): string[] {
  if (!isObject(e)) return [`${at} must be an object`];
  const problems: string[] = [];
  if (e.timeoutSeconds !== undefined && !positive(e.timeoutSeconds)) problems.push(`${at}.timeoutSeconds must be a positive integer`);
  if (!deployment) {
    for (const key of Object.keys(e)) if (key !== 'timeoutSeconds') problems.push(`${at}.${key} is set by the deployment, not an agent`);
    return problems;
  }
  if (e.env !== undefined && !(Array.isArray(e.env) && e.env.every((n) => typeof n === 'string'))) problems.push(`${at}.env must be a list of variable names`);
  const limits = e.limits;
  const known = ['addressSpaceMb', 'maxProcs', 'maxFileSizeMb'];
  if (limits !== undefined && !(isObject(limits) && Object.entries(limits).every(([k, v]) => known.includes(k) && Number.isInteger(v) && (v as number) >= 0))) problems.push(`${at}.limits must map ${known.join(', ')} to non-negative integers`);
  return problems;
}

function retentionProblems(r: unknown): string[] {
  return r === undefined || (isObject(r) && positive(r.idleDays) && Object.keys(r).length === 1) ? [] : ['retention must be { idleDays: positive integer }'];
}

function subagentProblems(s: unknown): string[] {
  if (s === undefined) return [];
  const known = ['maxConcurrent', 'maxTotal'];
  const ok = isObject(s) && Object.entries(s).every(([k, v]) => known.includes(k) && positive(v));
  return ok ? [] : [`subagents must map ${known.join(', ')} to positive integers`];
}

const ceilingProblems =(c: unknown) => (c === undefined || positive(c) ? [] : ['turnCeiling must be a positive integer']);

function validZone(z: unknown): boolean {
  try {
    return typeof z === 'string' && !!new Intl.DateTimeFormat('en', { timeZone: z });
  } catch {
    return false;
  }
}

function storeProblems(stores: unknown): string[] {
  if (stores === undefined) return [];
  if (!isObject(stores)) return ['stores must be an object'];
  return Object.entries(stores).flatMap(([name, s]) => {
    if (!isObject(s) || (s.kind !== 'sqlite' && s.kind !== 'postgres')) return [`stores.${name}.kind must be sqlite or postgres`];
    const extra = Object.keys(s).filter((k) => k !== 'kind' && !(s.kind === 'postgres' && k === 'urlEnv')).map((k) => `stores.${name}.${k} is not a ${s.kind} store key`);
    return s.kind === 'postgres' && typeof s.urlEnv !== 'string' ? [...extra, `stores.${name}.urlEnv must name the variable holding the postgres URL`] : extra;
  });
}

function deployedExtensionProblems(extensions: unknown, stores: unknown): string[] {
  if (extensions === undefined) return [];
  if (!isObject(extensions)) return ['extensions must be an object'];
  return Object.entries(extensions).flatMap(([name, e]) => {
    if (!isObject(e)) return [`extensions.${name} must be an object`];
    const problems = Object.keys(e).filter((k) => !['store', 'settings'].includes(k)).map((k) => `extensions.${name}.${k} is not an extension key`);
    if (e.store !== undefined && !(typeof e.store === 'string' && isObject(stores) && Object.hasOwn(stores, e.store))) problems.push(`extensions.${name}.store ${String(e.store)} is not in stores`);
    if (e.settings !== undefined && !isObject(e.settings)) problems.push(`extensions.${name}.settings must be an object`);
    return problems;
  });
}

const ROOT_KEYS = ['providers', 'defaultModel', 'timeZone', 'exec', 'subagents', 'stores', 'extensions'];

function rootProblems(raw: Record<string, unknown>): string[] {
  const problems = Object.keys(raw).filter((k) => !ROOT_KEYS.includes(k)).map((k) => (k === 'services' ? 'unknown top-level key services: it was renamed extensions, and an entry takes only store and settings' : `unknown top-level key ${k}`));
  if (!isObject(raw.providers) || Object.keys(raw.providers).length === 0) problems.push('providers must be an object naming at least one provider');
  else problems.push(...Object.entries(raw.providers).flatMap(([id, p]) => providerProblems(id, p)));
  if (typeof raw.defaultModel !== 'string' || !/^[^/]+\/.+$/.test(raw.defaultModel)) problems.push('defaultModel must be "provider/model"');
  if (raw.exec !== undefined) problems.push(...execProblems('exec', raw.exec, true));
  problems.push(...subagentProblems(raw.subagents));
  problems.push(...storeProblems(raw.stores), ...deployedExtensionProblems(raw.extensions, raw.stores));
  if (raw.timeZone !== undefined && !validZone(raw.timeZone)) problems.push('timeZone must be an IANA zone such as America/New_York');
  return problems;
}

/** Validates the deployment file's shape, reporting every problem at once. Model limits are not complete until `resolveDeployment` merges what the providers state. */
export function parseDeployment(raw: unknown): DeploymentConfig {
  const problems = isObject(raw) ? rootProblems(raw) : ['config must be an object'];
  if (problems.length) throw new Error(`invalid deployment config:\n- ${problems.join('\n- ')}`);
  return raw as DeploymentConfig;
}

const splitRef = (ref: string) => [ref.slice(0, ref.indexOf('/')), ref.slice(ref.indexOf('/') + 1)] as const;

/** What `GET {baseUrl}/models` states: ids always, and for llama.cpp upstreams `meta.n_ctx` and `architecture.input_modalities`. */
function listed(body: unknown): Record<string, ModelLimits> {
  const data = isObject(body) && Array.isArray(body.data) ? body.data : [];
  return Object.fromEntries(data.filter((m) => isObject(m) && typeof m.id === 'string').map((m) => {
    const nCtx = isObject(m.meta) ? m.meta.n_ctx : undefined;
    const modalities = isObject(m.architecture) && Array.isArray(m.architecture.input_modalities) ? m.architecture.input_modalities : undefined;
    return [m.id, { ...(positive(nCtx) && { contextWindow: nCtx as number }), ...(modalities && { images: modalities.includes('image') }) }];
  }));
}

async function discover(id: string, p: ProviderConfig, fetchFn: typeof fetch): Promise<Record<string, ModelLimits>> {
  // Classifier endpoints such as Clef list no models; their limits are always hand-written.
  if (p.api === 'systemone') return {};
  const key = process.env[providerEnv({ providers: { [id]: p } })[0]];
  try {
    const res = await fetchFn(`${p.baseUrl.replace(/\/$/, '')}/models`, { signal: AbortSignal.timeout(10_000), headers: key ? { authorization: `Bearer ${key}` } : {} });
    if (res.ok) return listed(await res.json());
    console.error(`providers.${id}: listing models failed with ${res.status}, using the hand-written ones`);
    return {};
  } catch (err) {
    console.error(`providers.${id}: could not list models, using the hand-written ones: ${err instanceof Error ? err.message : String(err)}`);
    return {};
  }
}

/** Fills each provider's models with the merge rule: discovered ids plus hand-written ids, a hand-written field winning; throws when defaultModel is not selectable (one with a contextWindow).
 * `only` limits discovery to those provider ids (see referencedProviders): the rest keep their hand-written models, and defaultModel is checked only when its provider is among them. */
export async function resolveDeployment(deployment: DeploymentConfig, fetchFn: typeof fetch = fetch, only?: readonly string[]): Promise<DeploymentConfig> {
  const wanted = (id: string) => !only || only.includes(id);
  const providers = Object.fromEntries(await Promise.all(Object.entries(deployment.providers).map(async ([id, p]) => {
    const models = wanted(id) ? await discover(id, p, fetchFn) : {};
    for (const [m, l] of Object.entries(p.models ?? {})) models[m] = { ...models[m], ...l };
    return [id, { ...p, models }] as const;
  })));
  const resolved = { ...deployment, providers };
  const problem = wanted(splitRef(resolved.defaultModel)[0]) ? modelProblems(resolved.defaultModel, resolved) : [];
  if (problem.length) throw new Error(`invalid deployment config:\n- defaultModel: ${problem.join('; ')}`);
  return resolved;
}

/** The deployment's providers an agent definition names: its model (the default when it names none), and any "provider/model" string or `{ provider }` object in its extension config. */
export function referencedProviders(definition: unknown, deployment: Pick<DeploymentConfig, 'providers' | 'defaultModel'>): string[] {
  const found = new Set<string>();
  const note = (id: unknown) => typeof id === 'string' && Object.hasOwn(deployment.providers, id) && found.add(id);
  const def = isObject(definition) ? definition : {};
  note(splitRef(typeof def.model === 'string' ? def.model : deployment.defaultModel)[0]);
  const walk = (v: unknown): void => {
    if (typeof v === 'string' && v.includes('/')) note(splitRef(v)[0]);
    else if (Array.isArray(v)) v.forEach(walk);
    else if (isObject(v)) {
      note(v.provider);
      Object.values(v).forEach(walk);
    }
  };
  walk(def.extensions);
  return [...found];
}

function modelProblems(model: unknown, deployment: DeploymentConfig): string[] {
  if (typeof model !== 'string' || !model.includes('/')) return ['model must be "provider/model"'];
  const limits = deployment.providers[splitRef(model)[0]]?.models?.[splitRef(model)[1]];
  if (!limits) return [`model ${model} is not configured in this deployment`];
  return limits.contextWindow ? [] : [`model ${model} has no contextWindow, so it cannot be selected; write one in providers.${splitRef(model)[0]}.models`];
}

/** What an agent installs: the names it selects plus their `requires` transitively, each after what it needs (selected `optional` ones too); problems name a missing requirement or a cycle. */
export function resolveExtensions(selected: readonly string[], catalog: Catalog): { order: string[]; problems: string[] } {
  const order: string[] = [];
  const problems: string[] = [];
  const state = new Map<string, 'visiting' | 'done'>();
  const visit = (name: string, path: string[]) => {
    if (state.get(name) === 'done') return;
    if (state.get(name) === 'visiting') return void problems.push(`extension dependency cycle: ${[...path.slice(path.indexOf(name)), name].join(' -> ')}`);
    state.set(name, 'visiting');
    const entry = catalog[name];
    for (const dep of [...(entry.requires ?? []), ...(entry.optional ?? []).filter((o) => selected.includes(o))]) {
      if (catalog[dep]) visit(dep, [...path, name]);
      else if (entry.requires?.includes(dep)) problems.push(`extension ${name} requires ${dep}, which is not available`);
    }
    state.set(name, 'done');
    order.push(name);
  };
  // Unknown selected names are reported by parseDefinition, not here.
  for (const name of selected) if (catalog[name]) visit(name, []);
  return { order, problems };
}

// Extensions a pinned snapshot may still select after they were removed; they load nothing.
const REMOVED_EXTENSIONS: Record<string, string> = { lsp: 'cut 2026-11-03 (#2086)' };

function withoutRemoved(raw: Record<string, unknown>): Record<string, unknown> {
  if (!isObject(raw.extensions)) return raw;
  const gone = Object.keys(raw.extensions).filter((name) => name in REMOVED_EXTENSIONS);
  for (const name of gone) console.error(`agent ${String(raw.name)}: extension ${name} was removed (${REMOVED_EXTENSIONS[name]}) and is ignored`);
  return gone.length ? { ...raw, extensions: Object.fromEntries(Object.entries(raw.extensions).filter(([name]) => !gone.includes(name))) } : raw;
}

function extensionProblems(extensions: unknown, deployment: DeploymentConfig, catalog: Catalog): string[] {
  if (!isObject(extensions)) return ['extensions must be an object of name to config'];
  const problems = Object.entries(extensions).flatMap(([name, config]) => catalog[name]
    ? (catalog[name].validate?.(config, deployment) ?? [])
    : [`unknown extension ${name}; available: ${Object.keys(catalog).sort().join(', ')}`]);
  return [...problems, ...resolveExtensions(Object.keys(extensions), catalog).problems];
}

/** Validates an agent definition against the deployment and the catalog, reporting every problem at once. */
export function parseDefinition(input: unknown, deployment: DeploymentConfig, catalog: Catalog): AgentDefinition {
  if (!isObject(input)) throw new Error('invalid agent definition:\n- definition must be an object');
  const raw = withoutRemoved(input);
  const problems: string[] = [];
  if (typeof raw.name !== 'string' || raw.name.length === 0) problems.push('name must be a non-empty string');
  if (!positive(raw.version)) problems.push('version must be a positive integer');
  const model = raw.model ?? deployment.defaultModel;
  problems.push(...modelProblems(model, deployment));
  // Pinned snapshots are immutable history and may still carry the removed key, so it is ignored, not rejected.
  if ('maxTurns' in raw) console.error(`agent ${String(raw.name)}: maxTurns was removed and is ignored`);
  if (raw.exec !== undefined) problems.push(...execProblems('exec', raw.exec, false));
  problems.push(...retentionProblems(raw.retention), ...ceilingProblems(raw.turnCeiling));
  if (raw.subagents !== undefined && raw.subagents !== false) problems.push('subagents must be false (omit it to keep the subagent tool)');
  if (raw.prompt !== undefined && (typeof raw.prompt !== 'string' || !raw.prompt)) problems.push('prompt must be a file name');
  problems.push(...extensionProblems(raw.extensions, deployment, catalog));
  if (problems.length) throw new Error(`invalid agent definition:\n- ${problems.join('\n- ')}`);
  return { ...raw, model } as AgentDefinition;
}

/** The variables the extensions an agent definition selects declare, for the session process to receive (and drop once read). */
export function declaredEnv(extensions: Record<string, unknown>, catalog: Catalog): string[] {
  return resolveExtensions(Object.keys(extensions), catalog).order.flatMap((name) => {
    const env = catalog[name].env;
    return typeof env === 'function' ? env(extensions[name]) : env ?? [];
  });
}

/** The variables a deployment's model providers read their API keys from. */
export function providerEnv(deployment: Pick<DeploymentConfig, 'providers'>): string[] {
  return Object.entries(deployment.providers).map(([id, p]) => p.apiKeyEnv ?? `QUACK_${id.toUpperCase().replace(/\W/g, '_')}_API_KEY`);
}

/** The configured price of a `provider/modelId` key, for pricing recorded tokens at today's rates. */
export const priceOf = (d: DeploymentConfig) => (key: string): Partial<Price> | undefined => {
  const i = key.indexOf('/');
  return d.providers[key.slice(0, i)]?.models?.[key.slice(i + 1)]?.price;
};

export function buildModels(deployment: DeploymentConfig): MutableModels {
  const models = createModels();
  const priced = (l: ModelLimits): Price => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, ...l.price });
  for (const [id, p] of Object.entries(deployment.providers)) {
    const entries = Object.entries(p.models ?? {}).filter(([, l]) => l.contextWindow);
    const auth = { apiKey: envApiKeyAuth(id, [providerEnv({ providers: { [id]: p } })[0]]) };
    if (p.api === 'systemone') {
      const list = entries.map(([m, l]) => ({ id: m, name: m, type: 'classifier', api: 'typesafe-system-one', provider: id, baseUrl: p.baseUrl, input: ['text'], cost: priced(l), contextWindow: l.contextWindow }));
      models.setProvider(createProvider({ id, baseUrl: p.baseUrl, auth, models: list as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }));
      continue;
    }
    // Without a hand-written maxTokens the model's context window caps a completion.
    const list = entries.map(([m, l]) => ({
      id: m, name: m, api: 'openai-completions', provider: id, baseUrl: p.baseUrl, reasoning: l.reasoning ?? false, input: l.images ? ['text', 'image'] : ['text'], cost: priced(l), contextWindow: l.contextWindow, maxTokens: l.maxTokens ?? l.contextWindow,
    }));
    models.setProvider(createProvider({ id, baseUrl: p.baseUrl, auth, models: list as never, api: openAICompletionsApi() }));
  }
  return models;
}

/** Extensions a deployment installs for every session, whatever its agent selects (a limit only works if every session obeys it). */
export type DeploymentExtensions = Array<(() => Extension[]) & { env?: readonly string[] }>;

/** The variables deployment-level extensions read, for the session process to receive (and drop once read). */
export const deploymentEnv = (always: DeploymentExtensions): string[] => always.flatMap((e) => e.env ?? []);

export async function agentSpec(def: AgentDefinition, catalog: Catalog, always: DeploymentExtensions = []): Promise<AgentSpec> {
  const slash = def.model.indexOf('/');
  return {
    model: { provider: def.model.slice(0, slash), modelId: def.model.slice(slash + 1) },
    ...(def.turnCeiling && { turnCeiling: def.turnCeiling }),
    ...(def.exec?.timeoutSeconds && { commandTimeoutSeconds: def.exec.timeoutSeconds }),
    extensions: [...(await Promise.all(resolveExtensions(Object.keys(def.extensions), catalog).order.map((name) => catalog[name](def.extensions[name])))).flat(), ...always.flatMap((make) => make())],
  };
}

/** Reads an agent definition file and the prompt file it names (resolved beside it) into an agent spec. */
export async function loadAgent(path: string, deployment: DeploymentConfig, catalog: Catalog, always: DeploymentExtensions = []): Promise<AgentSpec> {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(path, 'utf8'));
  } catch (err) {
    throw new Error(`reading ${path}: ${err instanceof Error ? err.message : String(err)}`);
  }
  const def = parseDefinition(raw, deployment, catalog);
  const instructions = def.prompt ? readFileSync(resolve(dirname(path), def.prompt), 'utf8') : undefined;
  return { ...(await agentSpec(def, catalog, always)), instructions };
}
