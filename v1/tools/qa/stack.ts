import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { cpSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { FAKE_MODEL, fakeModel } from './fake-model.ts';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const STATE = join(tmpdir(), 'quack-qa-stack.json');

export type AppSpec = 'stub' | 'real' | string;
export type StackOpts = {
  /** A built web dir, or `current` to build this tree's packages/web first. */
  web?: string;
  /** A directory of session data dirs to copy in, `demo` to create sessions through the fake model, or `none`. */
  seed?: string;
  apps?: Record<string, AppSpec>;
  /** `local` points the deployment at QA_MODEL_URL (default llm-swap); `none` and `fake` use a canned local endpoint. */
  model?: 'local' | 'none' | 'fake';
};
export type Stack = { url: string; dataDir: string; stop(): Promise<void> };

const STUB_BODIES: Record<string, unknown> = {
  schedules: { schedules: [
    { id: 'daily-digest', agent: 'sleeper-digest', sessionKey: 'sched:daily-digest', input: 'Post the morning digest', cron: '0 8 * * *', tz: 'America/New_York', source: 'agent', lastRun: '2026-11-03T13:00:00.000Z', nextRun: '2026-11-04T13:00:00.000Z', missedRuns: [] },
    { id: 'weekly-retro', agent: 'sleeper-retro', sessionKey: 'sched:weekly-retro', input: 'Write the weekly retro', cron: '0 9 * * 1', tz: 'America/New_York', source: 'api', lastRun: null, nextRun: '2026-11-09T14:00:00.000Z', missedRuns: [] },
  ] },
};

const DEMO_REVIEW = '# Review of PR 42\n\nApprove. The retry backoff now caps at 30 seconds and the tests cover the cap.\n\n- nit: rename `waitMs` to `backoffMs`\n';

/** Binds port 0 and releases it; a collision window remains, so callers start the listener immediately. */
export function freePort(): Promise<number> {
  return new Promise((ok, fail) => {
    const s = createServer().once('error', fail).listen(0, '127.0.0.1', () => {
      const { port } = s.address() as { port: number };
      s.close(() => ok(port));
    });
  });
}

async function waitFor(check: () => Promise<boolean>, what: string, ms = 60_000): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await check().catch(() => false)) return;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`timed out waiting for ${what}`);
}

const reachable = (url: string) => async () => (await fetch(url)).ok;

function stubServer(body: unknown, port: number): Promise<Server> {
  const server = createServer((_req, res) => res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(body)));
  return new Promise((ok) => server.listen(port, '127.0.0.1', () => ok(server)));
}

function deployment(modelUrl: string) {
  const limits = { contextWindow: 262144, maxTokens: 16384 };
  return {
    defaultModel: 'local/qwen3.8-27b',
    providers: {
      local: { baseUrl: modelUrl, models: { 'qwen3.8-27b': { ...limits, images: true }, [FAKE_MODEL]: limits } },
      clef: { baseUrl: modelUrl, api: 'systemone', models: { clef: { contextWindow: 8192, maxTokens: 1024 } } },
    },
  };
}

export function buildWeb(): string {
  const r = spawnSync('npm', ['run', 'build'], { cwd: join(ROOT, 'packages/web'), stdio: 'inherit' });
  if (r.status !== 0) throw new Error('web build failed');
  return join(ROOT, 'packages/web/dist');
}

function copySeed(from: string, dataDir: string): void {
  for (const name of readdirSync(from)) cpSync(join(from, name), join(dataDir, name), { recursive: true, filter: (src) => !/[\\/]workspace$/.test(src) });
}

type Parts = { children: ChildProcess[]; servers: Server[]; tmp: string };

function launch(parts: Parts, args: string[], env: Record<string, string>): ChildProcess {
  // A process group lets teardown also reach the session processes the distributor spawned.
  const child = spawn(process.execPath, args, { cwd: ROOT, env: { ...process.env, ...env }, stdio: ['ignore', 'inherit', 'inherit'], detached: true });
  parts.children.push(child);
  return child;
}

type Services = Record<string, { url: string; tokenEnv: string }>;

/** Starts each app (stubs at once, real schedules after the deployment file naming every service is written) and returns the services entries and their token variables. */
async function startApps(parts: Parts, specs: Record<string, AppSpec>, distributorUrl: string, config: string, base: object): Promise<{ env: Record<string, string> }> {
  const services: Services = {};
  const env: Record<string, string> = {};
  const real: Array<[string, number]> = [];
  for (const [name, spec] of Object.entries(specs)) {
    const port = await freePort();
    const tokenEnv = `QUACK_QA_${name.toUpperCase()}_TOKEN`;
    env[tokenEnv] = 'qa-token';
    services[name] = { url: `http://127.0.0.1:${port}`, tokenEnv };
    if (spec === 'real') real.push([name, port]);
    else parts.servers.push(await stubServer(spec === 'stub' ? STUB_BODIES[name] ?? {} : JSON.parse(readFileSync(spec, 'utf8')), port));
  }
  writeFileSync(config, JSON.stringify({ ...base, services }));
  for (const [name, port] of real) {
    if (name !== 'schedules') throw new Error(`only schedules has a real mode, not ${name}`);
    launch(parts, ['packages/schedules/src/main.ts'], { ...env, PORT: String(port), QUACK_CONFIG: config, QUACK_DATA_DIR: parts.tmp, QUACK_DISTRIBUTOR_URL: distributorUrl });
    await waitFor(async () => (await fetch(`http://127.0.0.1:${port}/`)).status > 0, `${name} service`);
  }
  return { env };
}

async function seedDemo(url: string): Promise<void> {
  const body = JSON.stringify({ input: 'Review PR 42: cap the retry backoff', wait: true });
  const res = await fetch(`${url}/sessions/qa-review/invocations?agent=quack`, { method: 'POST', body });
  if (!res.ok) throw new Error(`demo seed failed: ${res.status} ${await res.text()}`);
}

function killGroup(child: ChildProcess, signal: NodeJS.Signals): void {
  try { if (child.pid) process.kill(-child.pid, signal); } catch { /* already gone */ }
}

async function teardown(parts: Parts): Promise<void> {
  const exits = parts.children.map((c) => new Promise<void>((ok) => (c.exitCode !== null || c.signalCode ? ok() : c.once('exit', () => ok()))));
  for (const c of parts.children.slice().reverse()) c.kill('SIGTERM');
  await Promise.race([Promise.all(exits), new Promise((r) => setTimeout(r, 8_000))]);
  for (const c of parts.children) killGroup(c, 'SIGKILL');
  await Promise.all(parts.servers.map((s) => new Promise<void>((ok) => { s.closeAllConnections(); s.close(() => ok()); })));
  rmSync(parts.tmp, { recursive: true, force: true });
}

async function modelUrl(parts: Parts, opts: StackOpts): Promise<string> {
  const mode = opts.model ?? (opts.seed === 'demo' ? 'fake' : 'none');
  if (mode === 'local') return process.env.QA_MODEL_URL ?? 'http://127.0.0.1:11436/v1';
  // Port 9 (discard) refuses connections, so a model-free stack fails fast if anything calls the model.
  if (mode === 'none') return 'http://127.0.0.1:9/v1';
  const fake = await fakeModel(DEMO_REVIEW);
  parts.servers.push(fake.server);
  return fake.url;
}

/** Starts a distributor (and any app services) on free ports with a throwaway data dir; `stop` tears everything down. */
export async function startStack(opts: StackOpts = {}): Promise<Stack> {
  const parts: Parts = { children: [], servers: [], tmp: mkdtempSync(join(tmpdir(), 'quack-qa-')) };
  const stop = () => teardown(parts);
  const onExit = () => parts.children.forEach((c) => killGroup(c, 'SIGKILL'));
  process.once('exit', onExit);
  try {
    const web = !opts.web || opts.web === 'current' ? buildWeb() : resolve(opts.web);
    if (!existsSync(join(web, 'index.html'))) throw new Error(`no built web at ${web}`);
    const dataDir = join(parts.tmp, 'data');
    mkdirSync(dataDir);
    if (opts.seed && opts.seed !== 'none' && opts.seed !== 'demo') copySeed(resolve(opts.seed), dataDir);
    const config = join(parts.tmp, 'deployment.json');
    const port = await freePort();
    const url = `http://127.0.0.1:${port}`;
    const { env } = await startApps(parts, opts.apps ?? {}, url, config, deployment(await modelUrl(parts, opts)));
    launch(parts, ['packages/distributor/src/main.ts'], {
      ...env, PORT: String(port), QUACK_DATA_DIR: dataDir, QUACK_AGENTS_DIR: join(ROOT, 'agents'), QUACK_CONFIG: config, QUACK_WEB_DIR: web,
      QUACK_LOCAL_API_KEY: 'x', QUACK_CLEF_API_KEY: 'x',
    });
    await waitFor(reachable(`${url}/version`), 'the distributor');
    if (opts.seed === 'demo') await seedDemo(url);
    return { url, dataDir, stop: async () => { process.off('exit', onExit); await stop(); } };
  } catch (err) {
    onExit();
    await stop();
    throw err;
  }
}

function parseArgs(argv: string[]): StackOpts {
  const flag = (name: string) => argv[argv.indexOf(`--${name}`) + 1];
  const has = (name: string) => argv.includes(`--${name}`);
  const apps = has('apps') ? Object.fromEntries(flag('apps').split(',').map((kv) => kv.split('=') as [string, string])) : undefined;
  return { web: has('web') ? flag('web') : undefined, seed: has('seed') ? flag('seed') : undefined, apps, model: has('model') ? (flag('model') as StackOpts['model']) : undefined };
}

async function up(argv: string[]): Promise<void> {
  const stack = await startStack(parseArgs(argv));
  writeFileSync(STATE, JSON.stringify({ pid: process.pid, url: stack.url }));
  console.log(stack.url);
  const quit = () => void stack.stop().finally(() => { rmSync(STATE, { force: true }); process.exit(0); });
  process.on('SIGINT', quit);
  process.on('SIGTERM', quit);
}

async function down(): Promise<void> {
  if (!existsSync(STATE)) return console.log('no stack is up');
  const { pid } = JSON.parse(readFileSync(STATE, 'utf8')) as { pid: number };
  try { process.kill(pid, 'SIGINT'); } catch { rmSync(STATE, { force: true }); }
  await waitFor(async () => !existsSync(STATE), 'the stack to stop', 20_000);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2);
  if (cmd === 'up') await up(rest);
  else if (cmd === 'down') await down();
  else console.error('usage: node tools/qa/stack.ts up [--web <dir|current>] [--seed <dir|demo|none>] [--apps schedules=stub|real,sleeper=file.json] [--model local|none|fake] | down');
}
