export type RmDocument = { id: string; name: string; version: string; modified: string };
export type RmEntry = RmDocument & { folder: boolean; children?: RmEntry[] };

type Raw = { id: string; name: string; version?: number | string; lastModified?: string; isFolder?: boolean; children?: Raw[] };

export type RmClient = ReturnType<typeof createRmClient>;

// rmfakecloud UI API: login returns a plain-text JWT, and a 401 means it expired, so each call re-logs in once.
// Credentials stay in this closure and never reach an error message.
export function createRmClient(config: { url: string; email: string; password: string }) {
  const base = config.url.replace(/\/+$/, '');
  let token = '';

  async function login(): Promise<void> {
    const res = await fetch(`${base}/ui/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: config.email, password: config.password }) });
    if (!res.ok) throw new Error(`rmfakecloud login: status ${res.status}`);
    token = (await res.text()).trim();
  }

  async function get(path: string): Promise<Response> {
    const once = () => fetch(`${base}${path}`, { headers: { authorization: `Bearer ${token}` } });
    let res = await once();
    if (res.status === 401) {
      await login();
      res = await once();
    }
    if (!res.ok) throw new Error(`rmfakecloud GET ${path.split('?')[0]}: status ${res.status}`);
    return res;
  }

  // The tree may carry no version; lastModified changes on every save, so it stands in.
  const entry = (r: Raw): RmEntry => ({
    id: r.id, name: r.name, folder: r.isFolder === true, modified: r.lastModified ?? '',
    version: String(r.version ?? r.lastModified ?? ''),
    ...(r.isFolder ? { children: (r.children ?? []).map(entry) } : {}),
  });

  const tree = async (): Promise<RmEntry[]> => {
    const body = (await (await get('/ui/api/documents')).json()) as { Entries?: Raw[] };
    return (body.Entries ?? []).map(entry);
  };
  const walk = (es: RmEntry[]): RmDocument[] => es.flatMap((e) => (e.folder ? walk(e.children ?? []) : [{ id: e.id, name: e.name, version: e.version, modified: e.modified }]));

  return {
    /** The document tree with folders as nodes; trash is not part of it. */
    tree,
    /** Every document, folders flattened away. */
    documents: async (): Promise<RmDocument[]> => walk(await tree()),
    /** The document exported as PDF; the renderer works for notebooks, PDFs and epubs alike. */
    downloadPdf: async (id: string): Promise<Buffer> => {
      const pdf = Buffer.from(await (await get(`/ui/api/documents/${encodeURIComponent(id)}?type=pdf`)).arrayBuffer());
      // rmfakecloud answers 200 with an empty body, or an error page, when it cannot render a document.
      if (!pdf.subarray(0, 5).equals(Buffer.from('%PDF-'))) throw new Error(`rmfakecloud GET /ui/api/documents/${id}: ${pdf.length ? 'response is not a PDF' : 'empty PDF'}`);
      return pdf;
    },
  };
}
