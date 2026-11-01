import { createSign } from 'node:crypto';

export type AppConfig = { appId: string; privateKey: string; apiUrl?: string };

const TOKEN_SKEW_MS = 60_000;
const b64url = (data: string | Buffer) => Buffer.from(data).toString('base64url');

export class NoInstallation extends Error {}

// A GitHub App client: RS256 JWT for the app, then a cached installation token per repo, as v0 does (D56).
export function githubApp(config: AppConfig, fetchImpl: typeof fetch = fetch) {
  const api = (config.apiUrl ?? 'https://api.github.com').replace(/\/+$/, '');
  const installs = new Map<string, number>();
  const tokens = new Map<number, { token: string; expires: number }>();

  function appJwt(): string {
    const now = Math.floor(Date.now() / 1000);
    const head = b64url(JSON.stringify({ alg: 'RS256', typ: 'JWT' }));
    const body = b64url(JSON.stringify({ iat: now - 60, exp: now + 9 * 60, iss: config.appId }));
    const signature = createSign('RSA-SHA256').update(`${head}.${body}`).sign(config.privateKey);
    return `${head}.${body}.${b64url(signature)}`;
  }

  async function call(method: string, path: string, auth: string, body?: unknown) {
    const res = await fetchImpl(api + path, {
      method,
      headers: { authorization: auth, accept: 'application/vnd.github+json', 'x-github-api-version': '2022-11-28', 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (res.status === 404) return { status: 404, json: undefined, link: '' };
    if (!res.ok) throw new Error(`github ${method} ${path}: ${res.status} ${(await res.text()).slice(0, 300)}`);
    return { status: res.status, json: await res.json(), link: res.headers.get('link') ?? '' };
  }

  async function installationFor(owner: string, repo: string): Promise<number> {
    const key = `${owner}/${repo}`.toLowerCase();
    const known = installs.get(key);
    if (known) return known;
    const { status, json } = await call('GET', `/repos/${owner}/${repo}/installation`, `Bearer ${appJwt()}`);
    if (status === 404) throw new NoInstallation(`the app is not installed on ${owner}/${repo}`);
    installs.set(key, json.id);
    return json.id;
  }

  async function tokenFor(owner: string, repo: string): Promise<string> {
    const id = await installationFor(owner, repo);
    const cached = tokens.get(id);
    if (cached && Date.now() < cached.expires - TOKEN_SKEW_MS) return cached.token;
    const { json } = await call('POST', `/app/installations/${id}/access_tokens`, `Bearer ${appJwt()}`);
    if (!json?.token) throw new Error(`installation ${id} returned no token`);
    tokens.set(id, { token: json.token, expires: Date.parse(json.expires_at) });
    return json.token;
  }

  return {
    tokenFor,
    /** Calls the REST API as the app itself (JWT), for endpoints such as webhook deliveries. */
    async asApp(method: string, path: string) {
      return call(method, path, `Bearer ${appJwt()}`);
    },
    /** Calls the REST API as the installation that covers owner/repo. */
    async asInstallation(owner: string, repo: string, method: string, path: string, body?: unknown) {
      return call(method, path, `token ${await tokenFor(owner, repo)}`, body);
    },
  };
}

export type GithubApp = ReturnType<typeof githubApp>;
