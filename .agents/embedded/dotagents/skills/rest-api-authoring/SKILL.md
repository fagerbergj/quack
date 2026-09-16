---
name: rest-api-authoring
description: >
  Designs and reviews HTTP APIs and OpenAPI contracts: resource modeling, method and error
  semantics, schema composition, versioning, deprecation, compatibility, security, and
  validation. Use when creating or reviewing an OpenAPI document, planning an API version or
  sunset, assessing a compatibility change, or designing a REST-style endpoint.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0"
---

# REST API Design & OpenAPI Authoring

Design the contract before implementation. This skill produces a resource model, an OpenAPI contract, a compatibility decision, and validation evidence. It does not replace local API standards, threat modeling, or operational review.

## When to Use

Use for a new HTTP API, a substantial endpoint or schema design, an OpenAPI review, a versioning or deprecation plan, or a compatibility assessment between two API specifications.

## When NOT to Use

Do not use the full workflow for a documentation-only edit or a narrow implementation fix that does not change the API contract. Read the relevant reference instead. Follow an existing organizational API standard when one exists; this skill fills gaps rather than overriding it.

## Procedure

1. **Discover the house contract.** Find existing OpenAPI files, published API guidance, gateway routing, generated-client constraints, authentication conventions, supported OAS versions, and CDN/proxy cache-key settings. Reuse these choices unless the task explicitly changes them.
2. **Model resources and operations.** Identify resources as nouns, their representations, client roles, and state transitions. Map each operation to a path, HTTP method, success code, error code, and authorization rule. Read `references/richardson-model.md` when deciding the REST target; read `references/hateoas.md` when deciding whether runtime hypermedia controls are worth their cost; read `references/http-methods.md` for method, status, error, and link semantics.
3. **Write the OpenAPI contract.** Use shared components where reuse or local generator conventions justify them; inline one-off simple schemas when clearer. Keep request bodies separate from path/query/header/cookie parameters. Define security schemes centrally and apply concrete requirements per operation. Read `references/openapi-authoring.md` for OAS 3.1 schema, `$ref`, composition, example, and operation guidance. Read `references/migrating-to-openapi-3.1.md` only when migrating an OAS 3.0 document.
4. **Choose evolution policy.** Select versioning from the established contract; otherwise URI-path versioning is a practical default, subject to the real gateway and cache policy. Declare the migration path before shipping a breaking change. Read `references/api-versioning.md` for selection criteria, `references/api-deprecation.md` for RFC 9745/RFC 8594 signaling, and `references/schema-evolution.md` before changing a published contract.
5. **Review security per operation.** Record the required identity, scopes/roles, object/tenant checks, writable fields, resource limits, and external-call controls. A declared OpenAPI security scheme is not proof of authorization. Read `references/security.md` for the OWASP API Security Top 10 checklist.
6. **Validate and attach evidence.** Lint the resolved spec, compare it with the last published spec, and contract-test the implementation. Run `spectral lint <spec>`; run `oasdiff breaking <old-spec> <new-spec> --exit-code 1` for published-contract changes; run `schemathesis run <schema-url>` against a safe test environment. Read `references/tooling.md` when configuring these tools.

## Gotchas

- Do not apply OpenAPI 3.0 → 3.1 changes with global find-and-replace. `$ref`, composition, generated SDKs, and validators can change the safe migration path.
- JSON Schema `$ref` behavior applies inside Schema Objects. OpenAPI Reference Objects accept only `summary` and `description` siblings in OAS 3.1.
- Cache behavior is deployment configuration, not a property of version syntax. Verify the actual cache key and `Vary` handling.
- Do not expose internal authorization decisions, tokens, stack traces, or sensitive fields through Problem Details errors.

## Validation Loop

Before delivery, confirm that the resource model, operation-level security rules, API version/deprecation decision, and compatibility result are documented. Resolve lint errors, explain intentional warnings, run the compatibility gate for published APIs, and test the implementation against the same resolved specification. Repeat after any contract change.

## Resources

- Read `references/richardson-model.md` when choosing a REST maturity target or applying Fielding's constraints.
- Read `references/hateoas.md` when choosing hypermedia, a media type, or runtime discovery versus GraphQL introspection.
- Read `references/http-methods.md` when choosing methods, status codes, Problem Details, or link relations.
- Read `references/openapi-authoring.md` when authoring or reviewing an OAS 3.0/3.1 document, especially `$ref`, schemas, composition, or examples.
- Read `references/migrating-to-openapi-3.1.md` when upgrading an OAS 3.0 document.
- Read `references/api-versioning.md` when selecting a versioning strategy or checking gateway/cache implications.
- Read `references/api-deprecation.md` when announcing retirement or setting a sunset date.
- Read `references/schema-evolution.md` when comparing published contracts or classifying compatibility.
- Read `references/security.md` when reviewing authorization, limits, webhooks, or third-party API use.
- Read `references/tooling.md` when setting up lint, compatibility, or contract-test automation.
