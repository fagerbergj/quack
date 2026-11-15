import { createServer, type Server } from 'node:http';

/** A canned rmfakecloud: any login works and the tree holds one notebook and one folder, so the reMarkable app has a notebook to list. */
export function fakeRmfakecloud(): Promise<{ server: Server; url: string }> {
  const entries = [
    { id: 'qa-notes', name: 'Campaign Notes', version: 3, lastModified: '2026-10-01T10:00:00Z' },
    { id: 'qa-folder', name: 'Work', isFolder: true, lastModified: '2026-09-01T00:00:00Z', children: [{ id: 'qa-plan', name: 'Plan', version: 1, lastModified: '2026-09-02T00:00:00Z' }] },
  ];
  const server = createServer((req, res) => {
    req.resume();
    if (req.url === '/ui/api/login') return void res.writeHead(200).end('qa-jwt\n');
    if (req.url === '/ui/api/documents') return void res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ Entries: entries }));
    res.writeHead(404).end();
  });
  return new Promise((ok) => server.listen(0, '127.0.0.1', () => ok({ server, url: `http://127.0.0.1:${(server.address() as { port: number }).port}` })));
}
