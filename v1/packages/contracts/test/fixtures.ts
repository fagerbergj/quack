import type { ContractSpec } from '../src/index.ts';

const problem = (description: string) => ({ description, content: { 'application/problem+json': { schema: { $ref: '#/components/schemas/Problem' } } } });

/** GET /apps/sleeper/schedule as the Sleeper service serves it: the live week and each job's next run. */
export function scheduleDocument(at: 'iso' | 'epoch' = 'iso') {
  return {
    openapi: '3.1.0',
    info: { title: 'Sleeper schedule', version: String(at === 'iso' ? 1 : 2), description: 'Upcoming Sleeper jobs for the configured league.' },
    servers: [{ url: '/' }],
    security: [],
    paths: {
      '/apps/sleeper/schedule': {
        get: {
          operationId: 'getSleeperSchedule',
          summary: 'Upcoming runs',
          description: 'The live NFL week and the next run of each scheduled job, soonest first.',
          responses: {
            200: { description: 'The schedule', content: { 'application/json': { schema: { type: 'object', required: ['runs'], properties: {
              week: { type: 'integer', minimum: 1 },
              runs: { type: 'array', items: { type: 'object', required: ['job', 'at'], properties: { job: { type: 'string' }, at: at === 'iso' ? { type: 'string', format: 'date-time' } : { type: 'integer', description: 'Unix seconds' } } } },
            } } } } },
            501: problem('Schedules are not configured'),
            502: problem('The schedules service did not answer'),
          },
        },
      },
    },
    components: { schemas: { Problem: { type: 'object', properties: { title: { type: 'string' }, detail: { type: 'string' } } } } },
  };
}

export type Refs = { versioning: string; time: string };

export function scheduleSpecV1(refs: Refs): ContractSpec {
  return {
    kind: 'rest',
    title: 'Sleeper schedule endpoint',
    version: 1,
    intent: {
      purpose: 'Show the Sleeper page the live week and when each job runs next.',
      nonGoals: ['Editing schedules', 'Past runs'],
      audience: 'internal',
      consumers: ['The Sleeper page'],
      guarantees: ['Read-only', 'Errors say why in problem+json'],
    },
    document: scheduleDocument('iso'),
    choices: [
      { id: 'versioning', pointer: '/info/version', decision: 'Keep the existing unversioned route and evolve it compatibly.', why: [
        { source: 'answer', text: 'Keep the existing unversioned route', ref: refs.versioning },
        { source: 'guide', text: 'The house convention beats the AIP-185 default.', ref: 'rest#versioning' },
      ] },
      { id: 'time-format', pointer: '/paths/~1apps~1sleeper~1schedule/get/responses/200', decision: 'Run times are ISO 8601 date-times.', why: [
        { source: 'answer', text: 'You decide (the suggested ISO 8601 string stands)', ref: refs.time },
        { source: 'code', text: 'The service already returns the next run as the schedules service gives it.', ref: 'packages/sleeper/src/service.ts:85' },
      ] },
      { id: 'errors', pointer: '/paths/~1apps~1sleeper~1schedule/get/responses/502', decision: 'Failures are problem+json with 501 and 502.', why: [
        { source: 'guide', text: 'Errors are RFC 9457 problem+json.', ref: 'rest#errors' },
        { source: 'code', text: 'upcomingRuns answers 501 and 502.', ref: 'packages/sleeper/src/service.ts:79' },
      ] },
    ],
  };
}

export function scheduleSpecV2(refs: Refs, callers: string[]): ContractSpec {
  const v1 = scheduleSpecV1(refs);
  return {
    ...v1,
    version: 2,
    document: scheduleDocument('epoch'),
    choices: v1.choices.map((c) => (c.id === 'time-format' ? { ...c, decision: 'Run times are Unix seconds.', why: [...c.why, { source: 'answer', text: 'The change request asked for epoch seconds.', ref: 'request' }] } : c)),
    changes: [{ choice: 'time-format', was: 'ISO 8601 date-time string', now: 'Unix seconds integer', why: 'The change request asked for epoch seconds.' }],
    compatibility: { breaking: callers.length ? [{ what: 'runs[].at changes from a string to an integer', callers }] : [], safe: ['week and the other fields are unchanged'] },
  };
}

export const SLEEPER_PAGE = 'Sleeper page: packages/web/src/extensions/sleeper/api.ts:10 (upcoming)';

export function settingsSchema() {
  return {
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    title: 'Sleeper settings',
    type: 'object',
    description: 'Per-league settings the Sleeper extension reads at startup.',
    additionalProperties: false,
    required: ['leagueId'],
    properties: {
      leagueId: { type: 'string', description: 'Sleeper league id, opaque.' },
      mode: { enum: ['LINEUP', 'WAIVERS'], description: 'Which job runs by default.' },
      notifyTime: { type: 'string', format: 'date-time', description: 'When the weekly digest goes out, RFC 3339.' },
    },
  };
}

export function settingsSpec(): ContractSpec {
  return {
    kind: 'json-schema',
    title: 'Sleeper settings file',
    version: 1,
    intent: { purpose: 'The shape of the Sleeper settings file.', nonGoals: ['Secrets'], audience: 'internal', consumers: ['Sleeper extension'], guarantees: ['A typo in a key fails loudly'] },
    document: settingsSchema(),
    choices: [
      { id: 'closed', pointer: '/additionalProperties', decision: 'The file is closed: unknown keys are rejected.', why: [
        { source: 'guide', text: 'Config files fail loudly on a typo.', ref: 'json-schema#extensibility' },
        { source: 'code', text: 'The service takes the league from its own config, not from a request.', ref: 'packages/sleeper/src/service.ts:82' },
      ] },
    ],
  };
}
