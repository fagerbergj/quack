import type { Conversation, Extension, Harness, JsonObject } from '@earendil-works/pi-durable';
import type { Schema, Static } from '@earendil-works/pi-schema';
import type { IncomingMessage, ServerResponse } from 'node:http';
import type { SealedArtifact } from './artifacts.ts';
import type { DeploymentFile } from './extensions.ts';

/** A workflow an extension contributes: how to build it from an agent's binding, and the variables it reads from the session process. */
export type ContributedWorkflow = { build: (config: unknown) => unknown; env: readonly string[] };
export type ContributedWorkflows = Record<string, ContributedWorkflow>;

/** A submission's result: queued behind the run in hand, placed to start one, answered (the submitter waited), or unanswered with the reason. */
export type SubmitResult =
  | { submissionId: string; status: 'queued' | 'placed' }
  | { submissionId: string; status: 'done'; answer: string }
  | { submissionId: string; status: 'unanswered'; reason: string; detail?: string };

/** Takes a text submission over in place of the agent loop, as a bound workflow does; undefined leaves it to the loop. The session's submit calls the first extension naming one. */
export type Dispatch = (session: { harness: Harness; root: Conversation }, input: string, requestId: string | undefined, wait: boolean) => Promise<SubmitResult | undefined>;
/** Reruns the session's failed run from where it stopped; null when nothing failed. The session's resume calls the first extension naming one. */
export type Resume = (session: { harness: Harness; root: Conversation }) => Promise<SubmitResult | null>;
/** A session extension that takes submissions over (the workflows extension's bound workflow); the session finds it among the agent's extensions. */
export type DispatchingExtension = Extension & { dispatch?: Dispatch; resume?: Resume };

/** What an action's `start` is given: the distributor to submit to, and the variables the action declared, read before the session process drops them. */
export type ActionContext = { distributorUrl: string; fetch: typeof fetch; env: Record<string, string | undefined> };

/** Work an extension can start on a person's behalf. The home agent offers `summary` to the model and to the person who confirms; `start` calls the owner's own service or the distributor. */
export type Action<P extends Schema = Schema> = {
  name: string;
  /** One line: what starting it does. */
  summary: string;
  params: P;
  /** Variables `start` reads from `ctx.env`. */
  env?: readonly string[];
  start(params: Static<P>, ctx: ActionContext): Promise<{ session: string; title: string }>;
};

/** The slice of a deployment's config an extension may check its own config against. */
export type DeploymentView = { providers: Record<string, { models?: Record<string, { images?: boolean }> }> };

/** An extension's in-process half: it answers /apps/<name>/* with the path relative to its mount, and runs its background loops between start and stop. */
export type App = { handle(req: IncomingMessage, res: ServerResponse): Promise<unknown>; start?(): void | Promise<void>; stop?(): void | Promise<void>; /** Set from the manifest when the distributor mounts the app. */ pages?: PageRoute[] };

/** A route a credential-free request may reach: the app's UI routes and its public ingress (a webhook that signs its own payload). `path` is relative to the mount. */
export type PageRoute = { method: string; path: string | RegExp };

/** What the distributor gives an app: its deployment entry, database file, the process environment (secrets are read from it by name), and in-process access to the rest: `fetch` calls the distributor's own routes and `peerFetch(name)` another mounted app, neither over a socket or with a credential. `distributorUrl` is only the base those URLs resolve against. */
export type AppContext = { settings: unknown; dbPath: string; distributorUrl: string; fetch: typeof fetch; peerFetch(name: string): typeof fetch; deployment: DeploymentFile; env: NodeJS.ProcessEnv };

/** What an extension package exports as `manifest`. `session` installs it for agents selecting `name` (every session if `deployment`); `env` is what it reads from the process. */
export type ExtensionManifest = {
  name: string;
  session?: (config: unknown, workflows: ContributedWorkflows, actions: Action<any>[]) => Extension[] | Promise<Extension[]>;
  env?: readonly string[] | ((config: unknown, workflows: ContributedWorkflows, actions: Action<any>[]) => readonly string[]);
  /** Problems with an agent's config for this extension, given the deployment; empty when it is fine. */
  validate?: (config: unknown, deployment: DeploymentView) => string[];
  deployment?: boolean;
  /** Problems with `extensions.<name>.settings` of the deployment file, where name is this manifest's; the app checks the same settings at boot. Absent means it takes none. */
  settings?: (settings: unknown) => string[];
  /** Mounts the extension's app when the deployment file lists it under `extensions`. */
  app?: (ctx: AppContext) => App | Promise<App>;
  /** The routes of the app a credential-free request reaches; every other route needs a launch credential. */
  pages?: PageRoute[];
  /** Other apps this app calls through `peerFetch`; mounting refuses a deployment that lists it without them. */
  peers?: string[];
  /** Extensions this one cannot work without; an agent selecting it gets them too, loaded first. */
  requires?: string[];
  /** Extensions it uses when the agent also enables them (judge: results are checked); loaded before it, never added. */
  optional?: string[];
  workflows?: ContributedWorkflows;
  actions?: Action<any>[];
  evalAdapters?: EvalAdapter[];
};

/** A replay's stand-in for an extension's service half: it serves the pinned world and captures deliveries. */
export type EvalService = { env: Record<string, string>; captured: unknown[]; close(): Promise<void> };

/** What a recorded session holds that an adapter may build evidence from. */
export type RecordedSession = {
  docs: Record<string, JsonObject>;
  artifacts: Record<string, SealedArtifact[]>;
  /** The person's messages, in order. */
  inputs?: string[];
  /** The step results of the workflow run that sealed the judged version, by step name. */
  results?: JsonObject;
  /** A file of the session's workspace by its workspace-relative path; undefined when the recording kept none there. */
  readFile?: (path: string) => Promise<string | undefined>;
};

/** How eval replays and scores one artifact kind; the extension that owns the kind contributes it, so eval names no extension. */
export type EvalAdapter = {
  kind: string;
  /** Stubs by the document kind they stand in for; a pinned document of that kind is served by one during replay. */
  services?: Record<string, (pin: JsonObject) => Promise<EvalService>>;
  /** Whether the final artifact blocks what it judged; undefined when it holds no verdict. */
  blocks?: (content: string) => boolean | undefined;
  /** The kind's rubric (a @quack/judge Rubric, untyped here so sdk stays below judge) and either its declaration with the inputs rebuilt
   * from a recording, or its evidence rebuilt from one (a rubric not yet declared); either throws when the recording lacks what it needs. */
  judge?: { rubric: unknown; declaration?: unknown; inputs?: (session: RecordedSession, content: string) => Promise<Record<string, string>>; evidence?: (session: RecordedSession, content: string) => Promise<unknown> };
};
