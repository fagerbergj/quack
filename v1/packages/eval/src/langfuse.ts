import type { JsonValue } from '@earendil-works/chord';

export type LangfuseConfig = { baseUrl: string; publicKey: string; secretKey: string };
export type DatasetItem = { id: string; input: JsonValue; expectedOutput?: JsonValue; metadata?: JsonValue };

/** The Langfuse public API calls eval needs (EV1), on the endpoints v0's client proved against this deployment. */
export function langfuse(config: LangfuseConfig, fetchImpl: typeof fetch = fetch) {
  const auth = `Basic ${Buffer.from(`${config.publicKey}:${config.secretKey}`).toString('base64')}`;
  async function call(method: string, path: string, body?: unknown, ok404 = false) {
    const res = await fetchImpl(config.baseUrl.replace(/\/$/, '') + path, {
      method,
      headers: { authorization: auth, 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (ok404 && res.status === 404) return undefined;
    if (!res.ok) throw new Error(`langfuse ${method} ${path}: ${res.status} ${(await res.text()).slice(0, 300)}`);
    return res.json() as Promise<any>;
  }
  return {
    async ensureDataset(name: string) {
      if (!(await call('GET', `/api/public/v2/datasets/${encodeURIComponent(name)}`, undefined, true))) await call('POST', '/api/public/v2/datasets', { name });
    },
    /** Creates or replaces the item with this id. */
    upsertItem: (dataset: string, item: DatasetItem) => call('POST', '/api/public/dataset-items', { datasetName: dataset, ...item }),
    async items(dataset: string): Promise<DatasetItem[]> {
      const out: DatasetItem[] = [];
      for (let page = 1; ; page++) {
        const r = await call('GET', `/api/public/dataset-items?datasetName=${encodeURIComponent(dataset)}&page=${page}&limit=50`);
        out.push(...r.data);
        if (page >= (r.meta?.totalPages ?? 1)) return out;
      }
    },
    // Ingestion answers 207 with per-event failures, so any listed error fails the call.
    async ingest(batch: Array<{ id: string; type: string; body: Record<string, unknown> }>) {
      const timestamp = new Date().toISOString();
      const r = await call('POST', '/api/public/ingestion', { batch: batch.map((e) => ({ ...e, timestamp })) });
      if (r?.errors?.length) throw new Error(`langfuse ingest: ${r.errors.length} of ${batch.length} events failed; first ${r.errors[0].id}: ${r.errors[0].status} ${r.errors[0].message}`);
    },
    linkRun: (run: { runName: string; datasetItemId: string; traceId: string; metadata?: JsonValue }) => call('POST', '/api/public/dataset-run-items', run),
  };
}

export type Langfuse = ReturnType<typeof langfuse>;
