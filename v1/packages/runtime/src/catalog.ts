import { CodingTools } from '@earendil-works/pi-durable/tools';
import { githubSessionExtensions, type GithubSessionConfig } from '@quack/github';
import type { Catalog } from '@quack/session';

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required by the github extension`);
  return value;
}

// Extensions this runtime image carries, by the name agent definitions select them with (A8).
export const catalog: Catalog = {
  coding: () => [CodingTools],
  // The service secret comes from the environment, never the definition file (D56).
  github: (config) => githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: required('QUACK_GH_SERVICE_URL'), serviceToken: required('QUACK_GH_SERVICE_TOKEN') }),
};
