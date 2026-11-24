// Session models: what the deployment's providers offer, the Settings default, and each session's own pick, all kept in the data dir.
import { mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { join } from 'node:path';
import { sendProblem, serviceHttp } from '@quack/sdk';
import { availableModels, discoverModels, MODEL_FILE, resolveModel, type DeploymentConfig } from '@quack/session';
import { sessionDirName } from './process-driver.ts';

/** Runtime settings a person changes on the Settings page, in the data dir so they outlive restarts and need no deployment edit. */
export type Settings = { defaultModel?: string };
const SETTINGS_FILE = 'settings.json';
const NAME_MAX = 200;

/** One model a session may run, as provider/model with the limits its provider states or the deployment writes. */
export type ModelInfo = { name: string; contextWindow?: number; images?: boolean; reasoning?: boolean };

export type SessionModels = ReturnType<typeof sessionModels>;

/** `fetch` is the seam discovery calls each provider's `GET /models` through; a provider that cannot list falls back to its hand-written models. */
export function sessionModels(opts: { dataDir: string; deployment: DeploymentConfig; fetch?: typeof fetch }) {
  const settingsPath = join(opts.dataDir, SETTINGS_FILE);
  const pickPath = (key: string) => join(opts.dataDir, sessionDirName(key), MODEL_FILE);
  const settings = (): Promise<Settings> => readFile(settingsPath, 'utf8').then((t) => JSON.parse(t) as Settings, () => ({}));
  const discovered = () => discoverModels(opts.deployment, opts.fetch ?? fetch);
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
      const models = availableModels(resolved).map(({ name, contextWindow, images, reasoning }) => ({ name, contextWindow, ...(images && { images }), ...(reasoning && { reasoning }) }));
      return { models, ...(typeof fallback === 'string' && { defaultModel: fallback }) };
    },
    /** Sets the Settings default (null clears it); a string is why the model is refused. */
    async setDefault(model: string | null): Promise<string | undefined> {
      const why = model === null ? undefined : await refusal(model);
      if (why) return why;
      const next: Settings = { ...(await settings()), defaultModel: model ?? undefined };
      // Written aside and renamed, so a crash mid-write never leaves a torn file.
      await mkdir(opts.dataDir, { recursive: true });
      await writeFile(`${settingsPath}.tmp`, JSON.stringify(next));
      await rename(`${settingsPath}.tmp`, settingsPath);
    },
    sessionModel: (key: string): Promise<string | undefined> => readFile(pickPath(key), 'utf8').then((t) => t.trim() || undefined, () => undefined),
    /** Records the session's pick for its next run (null clears it); a string is why the model is refused. */
    async setSessionModel(key: string, model: string | null): Promise<string | undefined> {
      if (model === null) return void (await rm(pickPath(key), { force: true }));
      const why = await refusal(model);
      if (why) return why;
      await writeFile(pickPath(key), model);
    },
  };
}

const { send, json } = serviceHttp({ maxBody: 16 * 1024 });

const modelOf = (v: unknown): string | null | undefined =>
  v === null || (typeof v === 'string' && v.length > 0 && v.length <= NAME_MAX && v.includes('/')) ? v : undefined;

/** `GET /models`, `GET /settings` and `PATCH /settings`; false for any other request. */
export function modelRoutes(models: SessionModels) {
  async function patch(req: IncomingMessage, res: ServerResponse) {
    if (!req.headers['content-type']?.startsWith('application/json')) return sendProblem(res, 415, 'send application/json');
    const body = (await json(req).catch(() => null)) as { defaultModel?: unknown } | null;
    const model = modelOf(body?.defaultModel);
    if (model === undefined) return sendProblem(res, 400, 'the body must be { defaultModel: "provider/model" | null }');
    const why = await models.setDefault(model);
    if (why) return sendProblem(res, 400, why);
    send(res, 200, await models.settings());
  }
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const { pathname } = new URL(req.url ?? '', 'http://distributor');
    if (pathname === '/models' && req.method === 'GET') send(res, 200, await models.list());
    else if (pathname === '/settings' && req.method === 'GET') send(res, 200, await models.settings());
    else if (pathname === '/settings' && req.method === 'PATCH') await patch(req, res);
    else return false;
    return true;
  };
}
