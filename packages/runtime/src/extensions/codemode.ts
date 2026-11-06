import { CodemodeSandbox } from '@earendil-works/pi-codemode';
import type { Agent, Extension } from '@earendil-works/pi-durable';
import { createCodemodeExtension } from '@earendil-works/pi-durable/codemode';
import type { ExtensionManifest } from '@quack/sdk';

type Config = { tools?: string[]; timeoutMs?: number; memoryLimitMb?: number };

// A script reaches only the tools declared to its VM; narrowing `callable` keeps the rest out of the VM and the prompt section.
const narrow = (agent: Agent, allow?: string[]): Agent => (allow ? { ...agent, callable: agent.callable.filter((t) => allow.includes(t.name)) } : agent);

export const manifest: ExtensionManifest = {
  name: 'codemode',
  session: (config) => {
    const { tools, timeoutMs, memoryLimitMb } = (config ?? {}) as Config;
    const sandbox = new CodemodeSandbox({ ...(timeoutMs && { timeoutMs }), ...(memoryLimitMb && { memoryLimitBytes: memoryLimitMb * 1024 * 1024 }) });
    const ext = createCodemodeExtension({ sandbox, models: false });
    const wrapped: Extension = {
      ...ext,
      tools: ext.tools!.map((t) => ({
        ...t,
        execute: (args, api, ctx) => t.execute(args, Object.create(api, { agent: { value: async (c: typeof ctx) => narrow(await api.agent(c), tools) } }), ctx),
      })),
      sections: ext.sections!.map((s) => ({ ...s, render: (input, ctx) => s.render({ ...input, agent: narrow(input.agent, tools) }, ctx) })),
    };
    return [wrapped];
  },
};
