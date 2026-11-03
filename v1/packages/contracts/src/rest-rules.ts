import { Spectral, Document, type RulesetDefinition } from '@stoplight/spectral-core';
import Parsers from '@stoplight/spectral-parsers';
import { oas } from '@stoplight/spectral-rulesets';
import { truthy, pattern, undefined as absent, enumeration, schema } from '@stoplight/spectral-functions';

// RFC 9110 registered codes; anything else has no agreed meaning to a caller.
const REGISTERED = '100|101|102|103|200|201|202|203|204|205|206|207|208|226|300|301|302|303|304|305|307|308|400|401|402|403|404|405|406|407|408|409|410|411|412|413|414|415|416|417|421|422|423|424|425|426|428|429|431|451|500|501|502|503|504|505|506|507|508|510|511';
const OPERATIONS = '$.paths[*][get,put,post,delete,patch,options,head]';

// The adjudicated checks of the REST guide as Spectral rules, on top of the maintained oas ruleset.
const house: RulesetDefinition = {
  extends: [[oas as RulesetDefinition, 'recommended']],
  rules: {
    'quack-openapi-31': { description: 'The document is OpenAPI 3.1.x.', severity: 'error', given: '$.openapi', then: { function: pattern, functionOptions: { match: '^3\\.1\\.' } } },
    'quack-self-contained': { description: 'References stay inside the document; a remote ref can change under the contract.', severity: 'error', given: "$..['$ref']", then: { function: pattern, functionOptions: { match: '^#' } } },
    'quack-no-get-body': { description: 'GET, HEAD and DELETE carry no request body.', severity: 'error', given: '$.paths[*][get,head,delete]', then: { field: 'requestBody', function: absent } },
    'quack-registered-status': { description: 'Responses use registered status codes.', severity: 'error', given: '$.paths[*][*].responses', then: { field: '@key', function: pattern, functionOptions: { match: `^(default|[1-5]XX|${REGISTERED})$` } } },
    'quack-object-top-level': { description: 'A JSON response is an object, never a bare array, so fields can be added later.', severity: 'error', given: `${OPERATIONS}.responses[?(@property.match(/^2/))].content['application/json'].schema.type`, then: { function: enumeration, functionOptions: { values: ['object'] } } },
    'quack-problem-errors': { description: 'Errors are RFC 9457 application/problem+json.', severity: 'error', given: `${OPERATIONS}.responses[?(@property.match(/^[45]/))]`, then: { field: "content['application/problem+json']", function: truthy } },
    'quack-auth-recorded': { description: 'Auth is documented: a root security block (an empty list for a public API).', severity: 'error', given: '$', then: { field: 'security', function: truthy } },
    'quack-deprecation-text': { description: 'A deprecated operation says where callers go instead.', severity: 'error', given: OPERATIONS, then: { function: schema, functionOptions: { schema: { if: { required: ['deprecated'], properties: { deprecated: { const: true } } }, then: { required: ['description'] } } } } },
  },
};

export type Finding = { code: string; message: string; path: string };

/** Spectral findings at error severity for an OpenAPI document. */
export async function lintOpenApi(document: unknown): Promise<Finding[]> {
  const spectral = new Spectral();
  spectral.setRuleset(house);
  const results = await spectral.run(new Document(JSON.stringify(document), Parsers.Json));
  return results.filter((r) => r.severity === 0).map((r) => ({ code: String(r.code), message: r.message, path: `/${r.path.join('/')}` }));
}
