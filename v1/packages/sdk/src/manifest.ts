import type { Extension } from '@earendil-works/pi-durable';

/** A workflow an extension contributes: how to build it from an agent's binding, and the variables it reads from the session process. */
export type ContributedWorkflow = { build: (config: unknown) => unknown; env: readonly string[] };
export type ContributedWorkflows = Record<string, ContributedWorkflow>;

/**
 * What an extension package exports as `manifest`; the runtime lists packages in one place and builds its catalog from them.
 * `session` installs it for an agent that selects `name` (for every session when `deployment`); `env` is what it reads from the process.
 */
export type ExtensionManifest = {
  name: string;
  session?: (config: unknown, workflows: ContributedWorkflows) => Extension[] | Promise<Extension[]>;
  env?: readonly string[] | ((config: unknown, workflows: ContributedWorkflows) => readonly string[]);
  deployment?: boolean;
  workflows?: ContributedWorkflows;
};
