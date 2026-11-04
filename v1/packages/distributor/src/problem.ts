import { STATUS_CODES, type ServerResponse } from 'node:http';

/** An RFC 9457 problem reply. `error` repeats `detail` so clients written before problem+json keep working. */
export function sendProblem(res: ServerResponse, status: number, detail: string): void {
  const body = { type: 'about:blank', title: STATUS_CODES[status] ?? 'Error', status, detail, error: detail };
  res.writeHead(status, { 'content-type': 'application/problem+json' }).end(JSON.stringify(body));
}
