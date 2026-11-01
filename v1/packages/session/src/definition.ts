import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { createModels, createProvider, envApiKeyAuth, type MutableModels } from '@earendil-works/pi-ai';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import type { Extension } from '@earendil-works/pi-durable';
import type { ExecPolicy } from './exec.ts';
import type { AgentSpec } from './runtime.ts';

/** `images` declares image input; without it pi-ai replaces image blocks with a text placeholder before the request. */
type ModelLimits = { contextWindow: number; maxTokens?: number; reasoning?: boolean; images?: boolean };
/** `systemone` providers are decision models such as Clef: classifiers with an input cap and no generation (D54). */
type ProviderConfig = { baseUrl: string; api?: 'openai-completions' | 'systemone'; apiKeyEnv?: string; models: Record<string, ModelLimits> };

/** Per deployment: the OpenAI-compatible providers sessions may use (D28). */
export type DeploymentConfig = { providers: Record<string, ProviderConfig>; timeZone?: string; /** How agent shells are confined; see ExecPolicy. */ exec?: ExecPolicy };

/** Per agent (D48): its model as provider/model, a prompt file beside it, and the extensions it selects by name with their config. */
export type AgentDefinition = { name: string; version: number; model: string; prompt?: string; extensions: Record<string, unknown>; /** The longest one shell command may run for this agent. */ exec?: { timeoutSeconds?: number } };

/** An entry turns an agent's config into installed extensions; `env` names what it reads from the session process, which only agents selecting it receive. */
export type CatalogEntry = ((config: unknown) => Extension[] | Promise<Extension[]>) & { env?: readonly string[] | ((config: unknown) => readonly string[]) };
/** Extensions compiled into the runtime image, by name (A8). */
export type Catalog = Record<string, CatalogEntry>;

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const positive = (v: unknown) => typeof v === 'number' && Number.isInteger(v) && v > 0;

function providerProblems(id: string, p: unknown): string[] {
  if (!isObject(p)) return [`providers.${id} must be an object`];
  const problems: string[] = [];
  if (typeof p.baseUrl !== 'string' || !/^https?:\/\//.test(p.baseUrl)) problems.push(`providers.${id}.baseUrl must be an http(s) URL`);
  if (p.apiKeyEnv !== undefined && typeof p.apiKeyEnv !== 'string') problems.push(`providers.${id}.apiKeyEnv must be a string`);
  if (p.api !== undefined && p.api !== 'openai-completions' && p.api !== 'systemone') problems.push(`providers.${id}.api must be openai-completions or systemone`);
  if (!isObject(p.models) || Object.keys(p.models).length === 0) return [...problems, `providers.${id}.models must name at least one model`];
  return [...problems, ...Object.entries(p.models).flatMap(([m, l]) => limitProblems(`providers.${id}.models.${m}`, l, p.api === 'systemone'))];
}

function limitProblems(at: string, l: unknown, classifier: boolean): string[] {
  const ok = isObject(l) && positive(l.contextWindow) && (classifier || positive(l.maxTokens));
  return ok ? [] : [`${at} needs positive integer contextWindow${classifier ? '' : ' and maxTokens'}`];
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

function validZone(z: unknown): boolean {
  try {
    return typeof z === 'string' && !!new Intl.DateTimeFormat('en', { timeZone: z });
  } catch {
    return false;
  }
}

/** Validates the deployment config, reporting every problem at once. */
export function parseDeployment(raw: unknown): DeploymentConfig {
  const problems = !isObject(raw) || !isObject(raw.providers)
    ? ['config must have a providers object']
    : Object.entries(raw.providers).flatMap(([id, p]) => providerProblems(id, p));
  if (isObject(raw) && raw.exec !== undefined) problems.push(...execProblems('exec', raw.exec, true));
  if (isObject(raw) && raw.timeZone !== undefined && !validZone(raw.timeZone)) problems.push('timeZone must be an IANA zone such as America/New_York');
  if (problems.length) throw new Error(`invalid deployment config:\n- ${problems.join('\n- ')}`);
  return raw as DeploymentConfig;
}

function modelProblems(model: unknown, deployment: DeploymentConfig): string[] {
  if (typeof model !== 'string' || !model.includes('/')) return ['model must be "provider/model"'];
  const [provider, id] = [model.slice(0, model.indexOf('/')), model.slice(model.indexOf('/') + 1)];
  return deployment.providers[provider]?.models[id] ? [] : [`model ${model} is not configured in this deployment`];
}

function visionProblems(extensions: Record<string, unknown>, deployment: DeploymentConfig): string[] {
  const workflows = extensions.workflows;
  const ref = isObject(workflows) && isObject(workflows.visionModel) ? workflows.visionModel : undefined;
  if (!ref) return [];
  const { provider, modelId } = ref as { provider?: string; modelId?: string };
  const limits = typeof provider === 'string' && typeof modelId === 'string' ? deployment.providers[provider]?.models[modelId] : undefined;
  return limits?.images ? [] : [`workflows.visionModel ${String(provider)}/${String(modelId)} must be configured in this deployment with images: true`];
}

/** Validates an agent definition against the deployment and the catalog, reporting every problem at once. */
export function parseDefinition(raw: unknown, deployment: DeploymentConfig, catalog: Catalog): AgentDefinition {
  if (!isObject(raw)) throw new Error('invalid agent definition:\n- definition must be an object');
  const problems: string[] = [];
  if (typeof raw.name !== 'string' || raw.name.length === 0) problems.push('name must be a non-empty string');
  if (!positive(raw.version)) problems.push('version must be a positive integer');
  problems.push(...modelProblems(raw.model, deployment));
  // Pinned snapshots are immutable history and may still carry the removed key (D58), so it is ignored, not rejected.
  if ('maxTurns' in raw) console.error(`agent ${String(raw.name)}: maxTurns was removed (D58) and is ignored`);
  if (raw.exec !== undefined) problems.push(...execProblems('exec', raw.exec, false));
  if (raw.prompt !== undefined && (typeof raw.prompt !== 'string' || !raw.prompt)) problems.push('prompt must be a file name');
  if (!isObject(raw.extensions)) problems.push('extensions must be an object of name to config');
  else {
    for (const name of Object.keys(raw.extensions)) if (!catalog[name]) problems.push(`unknown extension ${name}; available: ${Object.keys(catalog).sort().join(', ')}`);
    problems.push(...visionProblems(raw.extensions, deployment));
  }
  if (problems.length) throw new Error(`invalid agent definition:\n- ${problems.join('\n- ')}`);
  return raw as AgentDefinition;
}

/** The variables the extensions an agent definition selects declare, for the session process to receive (and drop once read). */
export function declaredEnv(extensions: Record<string, unknown>, catalog: Catalog): string[] {
  return Object.entries(extensions).flatMap(([name, config]) => {
    const env = catalog[name]?.env;
    return typeof env === 'function' ? env(config) : env ?? [];
  });
}

/** The variables a deployment's model providers read their API keys from. */
export function providerEnv(deployment: DeploymentConfig): string[] {
  return Object.entries(deployment.providers).map(([id, p]) => p.apiKeyEnv ?? `QUACK_${id.toUpperCase().replace(/\W/g, '_')}_API_KEY`);
}

export function buildModels(deployment: DeploymentConfig): MutableModels {
  const models = createModels();
  const cost = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  for (const [id, p] of Object.entries(deployment.providers)) {
    const auth = { apiKey: envApiKeyAuth(id, [providerEnv({ providers: { [id]: p } })[0]]) };
    if (p.api === 'systemone') {
      const list = Object.entries(p.models).map(([m, l]) => ({ id: m, name: m, type: 'classifier', api: 'typesafe-system-one', provider: id, baseUrl: p.baseUrl, input: ['text'], cost, contextWindow: l.contextWindow }));
      models.setProvider(createProvider({ id, baseUrl: p.baseUrl, auth, models: list as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }));
      continue;
    }
    const list = Object.entries(p.models).map(([m, l]) => ({
      id: m, name: m, api: 'openai-completions', provider: id, baseUrl: p.baseUrl, reasoning: l.reasoning ?? false, input: l.images ? ['text', 'image'] : ['text'], cost, contextWindow: l.contextWindow, maxTokens: l.maxTokens,
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
    ...(def.exec?.timeoutSeconds && { commandTimeoutSeconds: def.exec.timeoutSeconds }),
    extensions: [...(await Promise.all(Object.entries(def.extensions).map(([name, config]) => catalog[name](config)))).flat(), ...always.flatMap((make) => make())],
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
