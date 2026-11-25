// Session models: what the deployment's providers offer, the Settings default, and each session's own pick, all kept in the data dir.
import { mkdir, readFile, rename, rm, stat, writeFile } from 'node:fs/promises';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { join } from 'node:path';
import { isModelRef } from '@quack/catalog';
import { principalName, sendProblem, serviceHttp, type Principal } from '@quack/sdk';
import { availableModels, discoverModels, MODEL_FILE, resolveModel, type DeploymentConfig } from '@quack/session';
import { sessionDirName } from './process-driver.ts';

/** Runtime settings a person changes on the Settings page, in the data dir so they outlive restarts and need no deployment edit. */
export type Settings = { defaultModel?: string };
const SETTINGS_FILE = 'settings.json';
// Long enough that a page of pickers and a burst of writes share one round of provider calls, short enough that a model swap shows within a minute.
const DISCOVERY_TTL_MS = 60_000;
// A round in which a provider could not list is kept only this long, so one hiccup does not hide its listed models for a minute.
const FAILED_TTL_MS = 5_000;

/** One model a session may run, as provider/model with the limits its provider states or the deployment writes; no `contextWindow` when neither states one. */
export type ModelInfo = { name: string; contextWindow?: number; images?: boolean; reasoning?: boolean };

export type SessionModels = ReturnType<typeof sessionModels>;

// Written aside and renamed, so a crash mid-write never leaves a torn file for the next read.
async function writeAside(path: string, text: string) {
  await writeFile(`${path}.tmp`, text);
  await rename(`${path}.tmp`, path);
}

/** `fetch` is the seam discovery calls each provider's `GET /models` through, `now` the clock its cache reads; a provider that cannot list falls back to its hand-written models. */
export function sessionModels(opts: { dataDir: string; deployment: DeploymentConfig; fetch?: typeof fetch; now?: () => number }) {
  const now = opts.now ?? Date.now;
  const settingsPath = join(opts.dataDir, SETTINGS_FILE);
  const sessionDir = (key: string) => join(opts.dataDir, sessionDirName(key));
  const settings = (): Promise<Settings> => readFile(settingsPath, 'utf8').then((t) => JSON.parse(t) as Settings, () => ({}));
  // Callers within the TTL share one discovery, one in flight included; discovery never rejects, a failing provider falls back instead.
  let cached: { at: number; failed?: boolean; value: Promise<DeploymentConfig> } | undefined;
  const discovered = () => {
    if (cached && now() - cached.at < (cached.failed ? FAILED_TTL_MS : DISCOVERY_TTL_MS)) return cached.value;
    const round: NonNullable<typeof cached> = { at: now(), value: Promise.resolve(opts.deployment) };
    const fetchFn = opts.fetch ?? fetch;
    const watched = (async (...args: Parameters<typeof fetch>) => {
      const res = await fetchFn(...args).catch((err: unknown) => { round.failed = true; throw err; });
      round.failed ||= !res.ok;
      return res;
    }) as typeof fetch;
    round.value = discoverModels(opts.deployment, watched);
    return (cached = round).value;
  };
  // A refusal names what is available, so the caller can correct it.
  async function refusal(model: string): Promise<string | undefined> {
    const names = availableModels(await discovered()).map((m) => m.name);
    return names.includes(model) ? undefined : `model ${model} is not available; available: ${names.join(', ') || 'none'}`;
  }
  return {
    settings,
    /** Every available model, and the default a session runs when neither it nor its agent names one. */
    async list(): Promise<{ models: ModelInfo[]; defaultModel?: string }> {
      const resolved = await discovered();
      const fallback = resolveModel(undefined, resolved, { default: (await settings()).defaultModel });
      const models = availableModels(resolved).map(({ name, contextWindow, images, reasoning }) => ({ name, ...(contextWindow && { contextWindow }), ...(images && { images }), ...(reasoning && { reasoning }) }));
      return { models, ...(typeof fallback === 'string' && { defaultModel: fallback }) };
    },
    /** The model a session's next run resolves to, as the runtime resolves it from the same inputs. */
    async resolved(pick: string | undefined, agentModel: string | undefined): Promise<string | undefined> {
      const model = resolveModel(agentModel, await discovered(), { session: pick, default: (await settings()).defaultModel });
      return typeof model === 'string' ? model : undefined;
    },
    /** Sets the Settings default (null clears it); a string is why the model is refused. */
    async setDefault(model: string | null): Promise<string | undefined> {
      const why = model === null ? undefined : await refusal(model);
      if (why) return why;
      await mkdir(opts.dataDir, { recursive: true });
      await writeAside(settingsPath, JSON.stringify({ ...(await settings()), defaultModel: model ?? undefined }));
    },
    sessionModel: (key: string): Promise<string | undefined> => readFile(join(sessionDir(key), MODEL_FILE), 'utf8').then((t) => t.trim() || undefined, () => undefined),
    /** Records the session's pick for its next run (null clears it); a string is why the model is refused. */
    async setSessionModel(key: string, model: string | null): Promise<string | undefined> {
      if (!(await stat(sessionDir(key)).catch(() => null))) return `session ${key} has no directory on this host, so it has nowhere to keep a model`;
      if (model === null) return void (await rm(join(sessionDir(key), MODEL_FILE), { force: true }));
      const why = await refusal(model);
      if (why) return why;
      await writeAside(join(sessionDir(key), MODEL_FILE), model);
    },
  };
}

const { send, json } = serviceHttp({ maxBody: 16 * 1024 });

/** `GET /models`, `GET /settings` and `PATCH /settings`; false for any other request. `who` is the caller a change is logged under. */
export function modelRoutes(models: SessionModels) {
  async function patch(req: IncomingMessage, res: ServerResponse, who: Principal | undefined) {
    if (!req.headers['content-type']?.startsWith('application/json')) return sendProblem(res, 415, 'send application/json');
    const model = ((await json(req).catch(() => null)) as { defaultModel?: unknown } | null)?.defaultModel;
    if (model !== null && !isModelRef(model)) return sendProblem(res, 400, 'the body must be { defaultModel: "provider/model" | null }');
    const why = await models.setDefault(model);
    if (why) return sendProblem(res, 400, why);
    console.log(`default model ${model === null ? 'cleared' : `set to ${model}`} by ${who ? principalName(who) : 'tailnet'}`);
    send(res, 200, await models.settings());
  }
  return async (req: IncomingMessage, res: ServerResponse, who?: Principal): Promise<boolean> => {
    const { pathname } = new URL(req.url ?? '', 'http://distributor');
    if (pathname === '/models' && req.method === 'GET') send(res, 200, await models.list());
    else if (pathname === '/settings' && req.method === 'GET') send(res, 200, await models.settings());
    else if (pathname === '/settings' && req.method === 'PATCH') await patch(req, res, who);
    else return false;
    return true;
  };
}
