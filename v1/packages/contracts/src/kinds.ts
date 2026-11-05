import { readFileSync } from 'node:fs';
import Parsers from '@stoplight/spectral-parsers';
import { schemaBreaks } from './json-schema-diff.ts';
import { lintJsonSchema } from './json-schema-rules.ts';
import { breakingChanges, type Break } from './oasdiff.ts';
import { lintOpenApi, type Finding } from './rest-rules.ts';
import type { ContractSpec } from './spec.ts';
import { typeScriptBreaks } from './typescript-diff.ts';
import { lintTypeScript } from './typescript-rules.ts';

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

export const jsonSchemaKind: ContractKind = {
  name: 'json-schema',
  skill: readFileSync(new URL('../skills/json-schema.md', import.meta.url), 'utf8'),
  extract: (source) => Parsers.Yaml.parse(source).data,
  check: lintJsonSchema,
  diff: schemaBreaks,
  renderHints: { layout: 'json-schema', export: { format: 'json-schema', filename: 'schema.json' } },
};

export const typescriptKind: ContractKind = {
  name: 'typescript',
  skill: readFileSync(new URL('../skills/typescript.md', import.meta.url), 'utf8'),
  // Only a .d.ts is read; a .ts file with initializers would fail its own check.
  extract: (source) => source,
  check: lintTypeScript,
  diff: typeScriptBreaks,
  renderHints: { layout: 'typescript', export: { format: 'typescript', filename: 'contract.d.ts' } },
};

export const kinds: Record<string, ContractKind> = { rest: restKind, 'json-schema': jsonSchemaKind, typescript: typescriptKind };
