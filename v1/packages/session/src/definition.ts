import { createModels, createProvider, envApiKeyAuth, type MutableModels } from '@earendil-works/pi-ai';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import type { Extension } from '@earendil-works/pi-durable';
import type { AgentSpec } from './runtime.ts';

type ModelLimits = { contextWindow: number; maxTokens: number; reasoning?: boolean };
type ProviderConfig = { baseUrl: string; apiKeyEnv?: string; models: Record<string, ModelLimits> };

/** Per deployment: the OpenAI-compatible providers sessions may use (D28). */
export type DeploymentConfig = { providers: Record<string, ProviderConfig> };

/** Per agent (D48): its model as provider/model, its turn limit, and the extensions it selects by name with their config. */
export type AgentDefinition = { name: string; version: number; model: string; maxTurns?: number; extensions: Record<string, unknown> };

/** Extensions compiled into the runtime image, by name (A8); each turns its config into installed extensions. */
export type Catalog = Record<string, (config: unknown) => Extension[]>;

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const positive = (v: unknown) => typeof v === 'number' && Number.isInteger(v) && v > 0;

function providerProblems(id: string, p: unknown): string[] {
  if (!isObject(p)) return [`providers.${id} must be an object`];
  const problems: string[] = [];
  if (typeof p.baseUrl !== 'string' || !/^https?:\/\//.test(p.baseUrl)) problems.push(`providers.${id}.baseUrl must be an http(s) URL`);
  if (p.apiKeyEnv !== undefined && typeof p.apiKeyEnv !== 'string') problems.push(`providers.${id}.apiKeyEnv must be a string`);
  if (!isObject(p.models) || Object.keys(p.models).length === 0) return [...problems, `providers.${id}.models must name at least one model`];
  for (const [m, l] of Object.entries(p.models)) {
    if (!isObject(l) || !positive(l.contextWindow) || !positive(l.maxTokens)) problems.push(`providers.${id}.models.${m} needs positive integer contextWindow and maxTokens`);
  }
  return problems;
}

/** Validates the deployment config, reporting every problem at once. */
export function parseDeployment(raw: unknown): DeploymentConfig {
  const problems = !isObject(raw) || !isObject(raw.providers)
    ? ['config must have a providers object']
    : Object.entries(raw.providers).flatMap(([id, p]) => providerProblems(id, p));
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
  if (!isObject(raw.extensions)) problems.push('extensions must be an object of name to config');
  else for (const name of Object.keys(raw.extensions)) if (!catalog[name]) problems.push(`unknown extension ${name}; available: ${Object.keys(catalog).sort().join(', ')}`);
  if (problems.length) throw new Error(`invalid agent definition:\n- ${problems.join('\n- ')}`);
  return raw as AgentDefinition;
}

export function buildModels(deployment: DeploymentConfig): MutableModels {
  const models = createModels();
  for (const [id, p] of Object.entries(deployment.providers)) {
    const list = Object.entries(p.models).map(([m, l]) => ({
      id: m, name: m, api: 'openai-completions', provider: id, baseUrl: p.baseUrl, reasoning: l.reasoning ?? false, input: ['text'],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: l.contextWindow, maxTokens: l.maxTokens,
    }));
    const auth = { apiKey: envApiKeyAuth(id, [p.apiKeyEnv ?? `QUACK_${id.toUpperCase().replace(/\W/g, '_')}_API_KEY`]) };
    models.setProvider(createProvider({ id, baseUrl: p.baseUrl, auth, models: list as never, api: openAICompletionsApi() }));
  }
  return models;
}

export function agentSpec(def: AgentDefinition, catalog: Catalog): AgentSpec {
  const slash = def.model.indexOf('/');
  return {
    model: { provider: def.model.slice(0, slash), modelId: def.model.slice(slash + 1) },
    maxTurns: def.maxTurns,
    extensions: Object.entries(def.extensions).flatMap(([name, config]) => catalog[name](config)),
  };
}
