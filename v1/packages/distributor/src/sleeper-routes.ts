import type { IncomingMessage, ServerResponse } from 'node:http';

export type SleeperRoutesConfig = { url: string; token: string; league: string; fetch?: typeof fetch };

const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

// The page reaches only the league id and the on-demand lineup job; the service token never reaches the browser.
export function sleeperRoutes(config: SleeperRoutesConfig) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const url = new URL(req.url ?? '', 'http://distributor');
    if (url.pathname === '/apps/sleeper' && req.method === 'GET') return send(res, 200, { league: config.league }), true;
    if (url.pathname !== '/apps/sleeper/lineup' || req.method !== 'POST') return false;
    const week = url.searchParams.get('week');
    if (week !== null && !/^\d{1,2}$/.test(week)) return send(res, 400, { error: 'bad week' }), true;
    try {
      const upstream = await (config.fetch ?? fetch)(`${config.url}/leagues/${encodeURIComponent(config.league)}/jobs/lineup${week ? `?week=${week}` : ''}`, {
        method: 'POST',
        headers: { authorization: `Bearer ${config.token}` },
      });
      send(res, upstream.status, await upstream.json().catch(() => ({})));
    } catch (err) {
      send(res, 502, { error: (err as Error).message });
    }
    return true;
  };
}
