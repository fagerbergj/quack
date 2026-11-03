import { readFileSync } from 'node:fs';
import Parsers from '@stoplight/spectral-parsers';
import { breakingChanges, type Break } from './oasdiff.ts';
import { lintOpenApi, type Finding } from './rest-rules.ts';
import type { ContractSpec } from './spec.ts';

/** A contract kind: how to read a contract in from code, check it, hint a renderer, and the guide its checks are judged against. */
export type ContractKind = {
  name: ContractSpec['kind'];
  /** The authoring guide; also the rubric source for the judged check. */
  skill: string;
  /** The kind's standard document from an existing contract file. */
  extract(source: string): unknown;
  /** Mechanical problems in the document. */
  check(document: unknown): Promise<Finding[]>;
  /** Changes between two versions' documents that break callers. */
  diff(prev: unknown, next: unknown): Promise<Break[]>;
  renderHints: { layout: string; export: { format: string; filename: string } };
};

export const restKind: ContractKind = {
  name: 'rest',
  skill: readFileSync(new URL('../skills/rest.md', import.meta.url), 'utf8'),
  // YAML is a superset of JSON, so one parser reads both.
  extract: (source) => Parsers.Yaml.parse(source).data,
  check: lintOpenApi,
  diff: breakingChanges,
  renderHints: { layout: 'rest', export: { format: 'openapi', filename: 'openapi.yaml' } },
};

export const kinds: Record<string, ContractKind> = { rest: restKind };
