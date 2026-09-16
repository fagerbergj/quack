# Schema Evolution — Breaking vs Backward-Compatible Changes

The fundamental rule: **a breaking change is any modification that can make an existing client fail without code changes** [oasdiff.com/docs/breaking-changes](https://www.oasdiff.com/docs/breaking-changes) → [specdrift.dev/guides/openapi-breaking-changes](https://specdrift.dev/guides/openapi-breaking-changes). This applies to the API contract defined in the OpenAPI spec, not just what a particular server happens to accept at runtime.

## Backward-Compatible (Additive) Changes

- Adding optional fields/properties to request or response objects (oasdiff classifies as *Info*: `new-optional-request-property`, `response-optional-property-added`).
- Widening types — e.g., changing a single-type field to an array, or adding enum values without removing existing ones (`list-of-types-widened`).
- Adding new endpoints or operations entirely.
- Making a nullable property non-null in *responses* is acceptable since clients that read the field remain compatible.

## Breaking Changes

- **Remove or rename required request fields** (`request-property-removed`, `new-required-request-property`).
- **Narrow types** — changing a property's type, narrowing an enum to fewer values, tightening constraints (decreasing `maxLength`, increasing `exclusiveMinimum`), making a nullable field non-null in *requests*.
- **Remove paths or operations entirely** (`api-path-removed-without-deprecation`).
- **Add required request parameters or properties** — existing clients that don't send the new value will fail validation.
- **Change security schemes** (remove a required auth method, modify its parameters).

## Input vs Output Schema Rule

The Zalando RESTful API Guidelines emphasize: never change the semantic of fields during evolution, and treat input vs. output schemas differently — additive changes are generally safe in responses but may break clients if they appear in request objects without defaults [github.com/zalando/restful-api-guidelines/blob/main/chapters/compatibility.adoc](https://github.com/zalando/restful-api-guidelines/blob/main/chapters/compatibility.adoc).

## Full Classification Catalog

oasdiff tracks **509 distinct API changes** across 10 OpenAPI areas (paths, components, schema, parameters, request bodies, responses, headers, security, tags, info), each classified as breaking/warning/informational [oasdiff.com/docs/breaking-changes](https://www.oasdiff.com/docs/breaking-changes) → [github.com/oasdiff/oasdiff/blob/main/docs/BREAKING-CHANGES.md](https://github.com/oasdiff/oasdiff/blob/main/docs/BREAKING-CHANGES.md).
