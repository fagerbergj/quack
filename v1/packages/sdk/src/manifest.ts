import type { Extension, JsonObject } from '@earendil-works/pi-durable';
import type { Schema, Static } from '@earendil-works/pi-schema';
import type { SealedArtifact } from './artifacts.ts';

/** A workflow an extension contributes: how to build it from an agent's binding, and the variables it reads from the session process. */
export type ContributedWorkflow = { build: (config: unknown) => unknown; env: readonly string[] };
export type ContributedWorkflows = Record<string, ContributedWorkflow>;

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
export type DeploymentView = { providers: Record<string, { models: Record<string, { images?: boolean }> }> };

/** What an extension package exports as `manifest`. `session` installs it for agents selecting `name` (every session if `deployment`); `env` is what it reads from the process. */
export type ExtensionManifest = {
  name: string;
  session?: (config: unknown, workflows: ContributedWorkflows, actions: Action<any>[]) => Extension[] | Promise<Extension[]>;
  env?: readonly string[] | ((config: unknown, workflows: ContributedWorkflows, actions: Action<any>[]) => readonly string[]);
  /** Problems with an agent's config for this extension, given the deployment; empty when it is fine. */
  validate?: (config: unknown, deployment: DeploymentView) => string[];
  deployment?: boolean;
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
export type RecordedSession = { docs: Record<string, JsonObject>; artifacts: Record<string, SealedArtifact[]> };

/** How eval replays and scores one artifact kind; the extension that owns the kind contributes it, so eval names no extension. */
export type EvalAdapter = {
  kind: string;
  /** Stubs by the document kind they stand in for; a pinned document of that kind is served by one during replay. */
  services?: Record<string, (pin: JsonObject) => Promise<EvalService>>;
  /** Whether the final artifact blocks what it judged; undefined when it holds no verdict. */
  blocks?: (content: string) => boolean | undefined;
  /** The kind's rubric (a @quack/judge Rubric, untyped here so sdk stays below judge) and its evidence rebuilt from a recording, or undefined when the artifact is not one this rubric judges. */
  judge?: { rubric: unknown; evidence: (session: RecordedSession, content: string) => Promise<unknown> };
};
