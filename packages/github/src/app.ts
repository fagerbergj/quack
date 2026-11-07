import { createSign } from 'node:crypto';
import { limiter } from '@quack/judge';

export type AppConfig = {
  appId: string; privateKey: string; apiUrl?: string;
  /** Per request, body included; default 30 s. */
  timeoutMs?: number;
  /** Requests in flight at once; GitHub's secondary limits punish bursts. Default 4. */
  maxConcurrent?: number;
  /** Waits out a rate limit; tests inject a fake. */
  sleep?: (ms: number) => Promise<void>;
};

const TOKEN_SKEW_MS = 60_000;
const TIMEOUT_MS = 30_000;
const MAX_CONCURRENT = 4;
const RATE_ATTEMPTS = 3;
// A longer wait is handed back to the caller as a 503 with Retry-After rather than held open.
const MAX_RATE_WAIT_MS = 60_000;
const SECONDARY_WAIT_MS = 60_000;
const b64url = (data: string | Buffer) => Buffer.from(data).toString('base64url');

export class NoInstallation extends Error {}

/** A non-2xx GitHub response, with its status so callers can tell a refusal from an outage. */
export class GithubError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** GitHub's rate limit outlasted the bounded retry; `retryAfterMs` is how long GitHub asked to wait. */
export class RateLimited extends GithubError {
  readonly retryAfterMs: number;
  constructor(status: number, retryAfterMs: number, message: string) {
    super(status, message);
    this.retryAfterMs = retryAfterMs;
  }
}

// Primary limit: 403/429 with x-ratelimit-remaining 0 until x-ratelimit-reset. Secondary: Retry-After, or a "rate limit" message and a minute's wait.
function rateLimitWait(res: Response, text: string, now: number): number | undefined {
  if (res.status !== 403 && res.status !== 429) return undefined;
  const after = res.headers.get('retry-after');
  if (after !== null && Number.isFinite(Number(after))) return Number(after) * 1000;
  const reset = Number(res.headers.get('x-ratelimit-reset') ?? NaN);
  if (res.headers.get('x-ratelimit-remaining') === '0') return Number.isFinite(reset) ? Math.max(0, reset * 1000 - now) : SECONDARY_WAIT_MS;
  return res.status === 429 || /rate limit/i.test(text) ? SECONDARY_WAIT_MS : undefined;
}

type Reply = { status: number; json: any; link: string };

// A GitHub App client: RS256 JWT for the app, then a cached installation token per repo, as v0 does.
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

  const slot = limiter(config.maxConcurrent ?? MAX_CONCURRENT);
  const sleep = config.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));

  // One attempt; a rate-limited answer comes back as its wait, so the retry sleeps outside the concurrency slot.
  async function attempt(method: string, path: string, auth: string, body?: unknown): Promise<Reply | { wait: number; error: RateLimited }> {
    try {
      const res = await fetchImpl(api + path, {
        method,
        // GraphQL has its own version scheme; the REST version header is not one it accepts.
        headers: { authorization: auth, accept: 'application/vnd.github+json', ...(path === '/graphql' ? {} : { 'x-github-api-version': '2022-11-28' }), 'content-type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(config.timeoutMs ?? TIMEOUT_MS),
      });
      if (res.status === 404) return { status: 404, json: undefined, link: '' };
      if (res.ok) return { status: res.status, json: await res.json(), link: res.headers.get('link') ?? '' };
      const text = await res.text();
      const message = `github ${method} ${path}: ${res.status} ${text.slice(0, 300)}`;
      const wait = rateLimitWait(res, text, Date.now());
      if (wait === undefined) throw new GithubError(res.status, message);
      return { wait, error: new RateLimited(res.status, wait, message) };
    } catch (err) {
      if (err instanceof GithubError) throw err;
      // A timeout or a dropped connection is an outage, which callers answer as a bad gateway.
      throw new GithubError(502, `github ${method} ${path}: ${(err as Error).name === 'TimeoutError' ? 'timed out' : (err as Error).message}`);
    }
  }

  async function call(method: string, path: string, auth: string, body?: unknown): Promise<Reply> {
    for (let n = 1; ; n++) {
      const r = await slot(() => attempt(method, path, auth, body));
      if (!('wait' in r)) return r;
      if (n >= RATE_ATTEMPTS || r.wait > MAX_RATE_WAIT_MS) throw r.error;
      await sleep(r.wait);
    }
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

  let slugPromise: Promise<string> | undefined;
  // The app's own handle: users mention the bot as @<slug>, and its comments are authored by <slug>[bot].
  const slug = () => (slugPromise ??= call('GET', '/app', `Bearer ${appJwt()}`).then((r) => String(r.json?.slug)).catch((err) => { slugPromise = undefined; throw err; }));

  return {
    tokenFor,
    slug,
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
