import { PROXY_PROOF } from '../../src/server.ts';

export const PROXY_SECRET = 'proxy-secret';
/** What the reverse proxy sends for a signed-in person: their identity and the proof that the proxy set it. */
export const PERSON = { 'x-authentik-username': 'jason', [PROXY_PROOF]: PROXY_SECRET };
