import type { ClassifierRef, ModelRef } from '@quack/judge';
import { ingestWorkflow, INGEST_WORKFLOW, type IngestService } from './workflow.ts';

export { ingestWorkflow, INGEST_WORKFLOW, NONE, type IngestDeps, type IngestService } from './workflow.ts';
export { problems, transcriptKind, TYPES, type Transcript } from './transcript.ts';
export { classifierChoice, parseTopic } from './classify.ts';
export { PAGE_WIDTH, renderPages } from './render.ts';

/** The service half over HTTP; the bearer token is the service's shared secret. */
export function createIngestService(url: string, token: string, doFetch: typeof fetch = fetch): IngestService {
  const call = async (path: string, init: RequestInit = {}) => {
    const res = await doFetch(`${url.replace(/\/+$/, '')}${path}`, { ...init, headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' } });
    if (!res.ok) {
      const why = ((await res.json().catch(() => null)) as { error?: string } | null)?.error;
      throw new Error(`remarkable service: ${init.method ?? 'GET'} ${path} returned ${res.status}${why ? `: ${why}` : ''}`);
    }
    return res;
  };
  return {
    pdf: async (id) => new Uint8Array(await (await call(`/documents/${encodeURIComponent(id)}/pdf`)).arrayBuffer()),
    topics: async () => ((await (await call('/topics')).json()) as { topics: string[] }).topics,
    addTopic: async (name) => void (await call('/topics', { method: 'POST', body: JSON.stringify({ name }) })),
  };
}

/** The workflow wired to the service half named in the environment (QUACK_REMARKABLE_URL, QUACK_REMARKABLE_TOKEN); the models come from the agent definition. */
export function ingestFromEnv(config: { visionModel?: ModelRef; classifier?: ClassifierRef } = {}, env: Record<string, string | undefined> = process.env) {
  const need = (name: string) => {
    const value = env[name];
    if (!value) throw new Error(`${name} is required by the ${INGEST_WORKFLOW} workflow`);
    return value;
  };
  return ingestWorkflow({ service: createIngestService(need('QUACK_REMARKABLE_URL'), need('QUACK_REMARKABLE_TOKEN')), visionModel: config.visionModel, classifier: config.classifier });
}
