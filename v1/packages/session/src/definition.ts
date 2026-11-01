import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { createModels, createProvider, envApiKeyAuth, type MutableModels } from '@earendil-works/pi-ai';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import type { Extension } from '@earendil-works/pi-durable';
import type { AgentSpec } from './runtime.ts';

type ModelLimits = { contextWindow: number; maxTokens?: number; reasoning?: boolean };
/** `systemone` providers are decision models such as Clef: classifiers with an input cap and no generation (D54). */
type ProviderConfig = { baseUrl: string; api?: 'openai-completions' | 'systemone'; apiKeyEnv?: string; models: Record<string, ModelLimits> };

/** Per deployment: the OpenAI-compatible providers sessions may use (D28). */
export type DeploymentConfig = { providers: Record<string, ProviderConfig>; timeZone?: string };

/** Per agent (D48): its model as provider/model, its turn limit, a prompt file beside it, and the extensions it selects by name with their config. */
export type AgentDefinition = { name: string; version: number; model: string; maxTurns?: number; prompt?: string; extensions: Record<string, unknown> };

/** Extensions compiled into the runtime image, by name (A8); each turns its config into installed extensions. */
export type Catalog = Record<string, (config: unknown) => Extension[] | Promise<Extension[]>>;

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
  if (isObject(raw) && raw.timeZone !== undefined && !validZone(raw.timeZone)) problems.push('timeZone must be an IANA zone such as America/New_York');
  if (problems.length) throw new Error(`invalid deployment config:\n- ${problems.join('\n- ')}`);
  return raw as DeploymentConfig;
}

function modelProblems(model: unknown, deployment: DeploymentConfig): string[] {
  if (typeof model !== 'string' || !model.includes('/')) return ['model must be "provider/model"'];
  const [provider, id] = [model.slice(0, model.indexOf('/')), model.slice(model.indexOf('/') + 1)];
  return deployment.providers[provider]?.models[id] ? [] : [`model ${model} is not configured in this deployment`];
}

/** Validates an agent definition against the deployment and the catalog, reporting every problem at once. */
export function parseDefinition(raw: unknown, deployment: DeploymentConfig, catalog: Catalog): AgentDefinition {
  if (!isObject(raw)) throw new Error('invalid agent definition:\n- definition must be an object');
  const problems: string[] = [];
  if (typeof raw.name !== 'string' || raw.name.length === 0) problems.push('name must be a non-empty string');
  if (!positive(raw.version)) problems.push('version must be a positive integer');
  problems.push(...modelProblems(raw.model, deployment));
  if (raw.maxTurns !== undefined && !positive(raw.maxTurns)) problems.push('maxTurns must be a positive integer');
  if (raw.prompt !== undefined && (typeof raw.prompt !== 'string' || !raw.prompt)) problems.push('prompt must be a file name');
  if (!isObject(raw.extensions)) problems.push('extensions must be an object of name to config');
  else for (const name of Object.keys(raw.extensions)) if (!catalog[name]) problems.push(`unknown extension ${name}; available: ${Object.keys(catalog).sort().join(', ')}`);
  if (problems.length) throw new Error(`invalid agent definition:\n- ${problems.join('\n- ')}`);
  return raw as AgentDefinition;
}

export function buildModels(deployment: DeploymentConfig): MutableModels {
  const models = createModels();
  const cost = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  for (const [id, p] of Object.entries(deployment.providers)) {
    const auth = { apiKey: envApiKeyAuth(id, [p.apiKeyEnv ?? `QUACK_${id.toUpperCase().replace(/\W/g, '_')}_API_KEY`]) };
    if (p.api === 'systemone') {
      const list = Object.entries(p.models).map(([m, l]) => ({ id: m, name: m, type: 'classifier', api: 'typesafe-system-one', provider: id, baseUrl: p.baseUrl, input: ['text'], cost, contextWindow: l.contextWindow }));
      models.setProvider(createProvider({ id, baseUrl: p.baseUrl, auth, models: list as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }));
      continue;
    }
    const list = Object.entries(p.models).map(([m, l]) => ({
      id: m, name: m, api: 'openai-completions', provider: id, baseUrl: p.baseUrl, reasoning: l.reasoning ?? false, input: ['text'], cost, contextWindow: l.contextWindow, maxTokens: l.maxTokens,
    }));
    models.setProvider(createProvider({ id, baseUrl: p.baseUrl, auth, models: list as never, api: openAICompletionsApi() }));
  }
  return models;
}

export async function agentSpec(def: AgentDefinition, catalog: Catalog): Promise<AgentSpec> {
  const slash = def.model.indexOf('/');
  return {
    model: { provider: def.model.slice(0, slash), modelId: def.model.slice(slash + 1) },
    maxTurns: def.maxTurns,
    extensions: (await Promise.all(Object.entries(def.extensions).map(([name, config]) => catalog[name](config)))).flat(),
  };
}

/** Reads an agent definition file and the prompt file it names (resolved beside it) into an agent spec. */
export async function loadAgent(path: string, deployment: DeploymentConfig, catalog: Catalog): Promise<AgentSpec> {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(path, 'utf8'));
  } catch (err) {
    throw new Error(`reading ${path}: ${err instanceof Error ? err.message : String(err)}`);
  }
  const def = parseDefinition(raw, deployment, catalog);
  const instructions = def.prompt ? readFileSync(resolve(dirname(path), def.prompt), 'utf8') : undefined;
  return { ...(await agentSpec(def, catalog)), instructions };
}
