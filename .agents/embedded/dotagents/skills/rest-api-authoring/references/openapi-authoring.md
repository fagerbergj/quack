# OpenAPI Spec Authoring Patterns

## Overview

The authoritative source is the OAI/OpenAPI-Specification, currently at version **3.1.1** [swagger.io/specification](https://swagger.io/specification/). Maintained by the OpenAPI Initiative under SmartBear with OAS Working Group contributions.

Key root-level sections:
- **`openapi`**: Required version field; `major.minor` designates feature set, `.patch` addresses errors only [swagger.io/specification/#versions](https://swagger.io/specification/).
- **`paths`**: Maps URL path templates to Path Item Objects. Paths must start with `/` [learn.openapis.org/specification/paths](https://learn.openapis.org/specification/paths.html).
- **`components`**: Container for reusable definitions — schemas, parameters, responses, examples, requestBodies, headers, securitySchemes, links — referenced via `$ref` [learn.openapis.org/specification/components](https://learn.openapis.org/specification/components.html).
- **`securitySchemes`**: Defines authentication mechanisms (apiKey, http, mutualTLS, oauth2, openIdConnect) under `components`, never declared inline; always referenced via Security Requirement Object at global or operation level [learn.openapis.org/specification/security](https://learn.openapis.org/specification/security.html).
- **`servers`**: Array of Server Objects with base URLs, supporting variable portions (`{protocol}`, `{host}`) with `variables` maps [learn.openapis.org/specification/servers](https://learn.openapis.org/specification/servers.html).
- **`webhooks`** (3.1+): Top-level object for first-class webhook descriptions [specway.com/blog/openapi-3-1-vs-3-0](https://specway.com/blog/openapi-3-1-vs-3-0).

## OAS 3.0 vs 3.1 Key Differences

OpenAPI 3.1 (released February 2021) achieves full compatibility with **JSON Schema Draft 2020-12** [learn.openapis.org/upgrading/v3.0-to-v3.1](https://learn.openapis.org/upgrading/v3.0-to-v3.1.html).

| Change | OAS 3.0 | OAS 3.1+ |
|--------|---------|----------|
| **JSON Schema version** | Modified Draft-04 subset | Full Draft 2020-12 [learn.openapis.org/upgrading/v3.0-to-v3.1](https://learn.openapis.org/upgrading/v3.0-to-v3.1.html) |
| **Nullable** | `nullable: true` keyword | Removed; use `type: ["string", "null"]` array [specway.com/blog/openapi-3-1-vs-3-0](https://specway.com/blog/openapi-3-1-vs-3-0) → [apinotes.io/blog/common-openapi-spec-errors](https://apinotes.io/blog/common-openapi-spec-errors-and-how-to-fix-them) |
| **exclusiveMinimum/Maximum** | Boolean modifiers (`minimum: 7, exclusiveMinimum: true`) | Direct numeric values (`exclusiveMinimum: 7`) [learn.openapis.org/upgrading/v3.0-to-v3.1](https://learn.openapis.org/upgrading/v3.0-to-v3.1.html) |
| **Reference Object `$ref` siblings** | Adjacent keywords ignored | Only `summary` and `description` are allowed; other siblings are ignored [OAS 3.1.1](https://spec.openapis.org/oas/v3.1.1.html) |
| **Schema Object `$ref` behavior** | Logical replacement | JSON Schema semantics apply; sibling JSON Schema and OAS Schema Object keywords are valid [OAS 3.1.1](https://spec.openapis.org/oas/v3.1.1.html) |
| **$defs / $schema** | Not natively supported | Full JSON Schema 2020-12 support; default dialect is `https://spec.openapis.org/oas/3.1/dialect/base` [specway.com/blog/openapi-3-1-vs-3-0](https://specway.com/blog/openapi-3-1-vs-3-0) |
| **Type as array** | Always a string | Union types allowed: `type: ["string", "integer"]` [learn.openapis.org/upgrading/v3.0-to-v3.1](https://learn.openapis.org/upgrading/v3.0-to-v3.1.html) |

> **Critical warning:** The removal of `nullable` is the most common migration issue — every schema using `nullable: true` needs rewriting to type arrays [specway.com/blog/openapi-3-1-vs-3-0](https://specway.com/blog/openapi-3-1-vs-3-0). The `exclusiveMinimum` boolean→numeric change generates the highest error count across 7,000+ analyzed specs [apinotes.io/blog/common-openapi-spec-errors](https://apinotes.io/blog/common-openapi-spec-errors-and-how-to-fix-them).

## Schema Composition Patterns

### allOf for composition

Combine shared base properties with schema-specific extensions [swagger.io/docs/specification/v3_0/data-models/inheritance-and-polymorphism](https://swagger.io/docs/specification/v3_0/data-models/inheritance-and-polymorphism/). Validation applies the combined model against each subschema. Avoid conflicting property names.

### oneOf / anyOf with discriminator

- **oneOf** (preferred): Data must conform to exactly one schema — predictable validation [redocly.com/learn/openapi/any-of-one-of](https://redocly.com/learn/openapi/any-of-one-of).
- **anyOf**: Data may match one or more schemas, causing parsing ambiguity; prefer `oneOf` unless a union-type constraint genuinely requires it.

The **discriminator** keyword points to a property that identifies the variant type, works with `anyOf` or `oneOf` only, and all referenced models must contain it [swagger.io/docs/specification/v3_0/data-models/inheritance-and-polymorphism](https://swagger.io/docs/specification/v3_0/data-models/inheritance-and-polymorphism/). Some consider it redundant in 3.1 since JSON Schema's own validation handles the same logic, but it remains OAS-native and tooling-friendly [github.com/OAI/OpenAPI-Specification/discussions/3951](https://github.com/OAI/OpenAPI-Specification/discussions/3951) → [bump.sh/blog/the-discriminator-in-openapi-is-generally-redundant](https://bump.sh/blog/the-discriminator-in-openapi-is-generally-redundant-and-confusing/).

### $ref resolution and siblings

First identify the object containing `$ref`:

- **Schema Object**: OAS 3.1 uses JSON Schema semantics. Sibling JSON Schema keywords apply alongside `$ref`; OAS Schema Object fields such as `discriminator` are also valid.
- **OpenAPI Reference Object**: only `summary` and `description` are allowed beside `$ref`. Other properties must be ignored by compliant tooling [OAS 3.1.1](https://spec.openapis.org/oas/v3.1.1.html).

Do not rely on a Schema Object technique to extend a referenced Parameter, Response, Path Item, or other OAS object. Create a new object or use the target object's native composition mechanism instead.

References resolve against the retrieval URI of the document containing the reference. Test resolved multi-file specs in the exact bundler/validator used by CI; inconsistent base-URI handling is a common pitfall [github.com/OAI/OpenAPI-Specification/discussions/2739](https://github.com/OAI/OpenAPI-Specification/discussions/2739).

### patternProperties

Available natively in OAS 3.1 (inherited from JSON Schema 2020-12) for defining schemas keyed by regex patterns on property names [specway.com/blog/openapi-3-1-vs-3-0](https://specway.com/blog/openapi-3-1-vs-3-0). Not available in OAS 3.0.

## Operation Object Best Practices

Response Objects require a `description` explaining the status code in context. Operation `summary` and `description` fields, a ≤50-character summary limit, and parameter ordering are useful house-style rules, not OAS validity requirements. Apply the repository's lint rules and generator conventions first [learn.openapis.org/specification/paths](https://learn.openapis.org/specification/paths.html).

Place **required** parameters before optional ones in the `parameters` array [apimatic.io/blog/2022/11/14-best-practices-to-write-openapi](https://www.apimatic.io/blog/2022/11/14-best-practices-to-write-openapi-for-better-api-consumption).

OAS 3 separates `requestBody` (payload) from `parameters` (path/query/header/cookie inputs); defining body parameters inside `parameters` is the most common migration mistake when moving from Swagger 2.0 to OAS 3 [swagger.io/docs/specification/v3_0/describing-request-body](https://swagger.io/docs/specification/v3_0/describing-request-body/).

At least one response is mandatory (recommended to be the success case, typically 200); wildcard status codes (`1XX`, `2XX`, etc.) are supported with explicit codes taking precedence [learn.openapis.org/specification/paths](https://learn.openapis.org/specification/paths.html). Place the catch-all `default` response at the bottom.

## Examples and $ref Usage

Reusable Example Objects live under `components/examples` with a `value` and optional `summary`/`description` [swagger.io/docs/specification/v3_0/components](https://swagger.io/docs/specification/v3_0/components/). Use embedded `example` (singular) for single-use cases; use `examples:` (plural) with `$ref` for multiple named examples per content type, enabling reusability and tooling that picks a specific example by name.

In 3.0, `examples` values need a `value:` key for the example data — avoid adding redundant keys when using `$ref` inside content examples [stackoverflow.com/questions/59949082](https://stackoverflow.com/questions/59949082). Referencing examples across methods with the same `$ref` has historically caused issues in some tooling versions [github.com/OAI/OpenAPI-Specification/issues/3198](https://github.com/OAI/OpenAPI-Specification/issues/3198).

## Common Pitfalls

| Pitfall | Impact |
|---------|--------|
| **Duplicated complex schemas** | Create maintenance debt and drift; extract to `components/schemas` when reuse or local generator conventions justify it |
| **Parameter duplication** (path + query) | Confuses SDK generators [stackoverflow.com/questions/66381503](https://stackoverflow.com/questions/66381503) |
| **Schema mutation from reuse** | Changes to shared `$ref` schemas propagate everywhere; version carefully |
| **Missing examples on error responses** | Reduces developer experience in generated docs and mock servers |
| **oneOf validation failures** | Most common error across 7,000+ analyzed specs (19,772 occurrences) — use discriminators or ensure clear schema boundaries [apinotes.io/blog/common-openapi-spec-errors](https://apinotes.io/blog/common-openapi-spec-errors-and-how-to-fix-them) |
| **Broken $ref resolution** | Second most common error; references are processed relative to the **processing document**, not the file containing the reference |
| **Empty `servers: []`** | Causes tools to default to localhost or example domains, breaking consumption |
