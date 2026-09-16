# Migrating from OpenAPI 3.0 to 3.1

Sources: [OpenAPI.org migration guide](https://www.openapis.org/blog/2021/02/16/migrating-from-openapi-3-0-to-3-1-0), [OAS 3.1.1](https://spec.openapis.org/oas/v3.1.1.html), and [OpenAPI learning guide](https://learn.openapis.org/upgrading/v3.0-to-v3.1.html).

OAS 3.1 aligns Schema Objects with JSON Schema 2020-12. The version change can affect validators, generators, and references even when the API's intended behavior has not changed. Migrate a resolved spec and test downstream tooling; do not use a global find-and-replace.

## Migration procedure

1. Upgrade `openapi: 3.0.x` to a supported `3.1.x` version in a branch.
2. Resolve external references, then inventory Schema Objects using `nullable`, boolean `exclusiveMinimum`/`exclusiveMaximum`, Schema Object `example`, file `format`s, and `$ref` siblings.
3. Convert each schema deliberately, preserving its accepted and emitted instances. For each changed schema, validate examples and regenerate at least one affected client/server artifact.
4. Run `spectral lint`, compare the old and migrated contracts with `oasdiff breaking`, and run contract tests against a test deployment.
5. Upgrade only after the deployed tooling supports the target OAS 3.1 version.

If the document is Swagger 2.0 (`swagger: 2.0`), first convert it to OAS 3.0 with [swagger2openapi](https://github.com/Mermade/oas-kit/tree/main/packages/swagger2openapi), validate it, then perform this migration.

## Schema changes

### `nullable` becomes a union type

`nullable` was removed from the OAS 3.1 Schema Object. Express nullability with a type union when the schema's type is local and explicit:

```yaml
# OAS 3.0
type: string
nullable: true

# OAS 3.1
type: [string, 'null']
```

Do not mechanically add `type` next to a `$ref` or composition. Establish which branch permits null, then model that explicitly, for example:

```yaml
anyOf:
  - $ref: '#/components/schemas/Address'
  - type: 'null'
```

### Numeric exclusive bounds

OAS 3.0 used boolean modifiers:

```yaml
minimum: 7
exclusiveMinimum: true
```

OAS 3.1 uses the numeric bound directly:

```yaml
exclusiveMinimum: 7
```

Convert `exclusiveMinimum: false` to `minimum`, and make the analogous choice for `maximum`/`exclusiveMaximum`. Check generated validation code because this changes keyword shape.

### Schema Object `example` is deprecated, not removed

OAS 3.1 retains Schema Object `example` as a deprecated compatibility field and favors JSON Schema `examples`:

```yaml
# Compatible, but deprecated in OAS 3.1
example: fedora

# Preferred JSON Schema form
examples: [fedora, ubuntu]
```

Modernize to `examples` when the consuming tools support it. Retain `example` temporarily when a published documentation renderer or generator requires it. Media Type Object `example` and `examples` are separate OpenAPI constructs; do not rewrite those based on this rule.

### File and encoded content annotations are not drop-in replacements

`format: binary`, `format: byte`, and `format: base64` remain annotations that existing OpenAPI tools commonly interpret. `contentEncoding` describes an encoded string; `contentMediaType` identifies the media type of the decoded content. Use them only when those semantics describe the payload.

```yaml
# A base64-encoded string
schema:
  type: string
  contentEncoding: base64
  contentMediaType: image/png
```

For an `application/octet-stream` request body, an empty Media Type Object can describe an unconstrained stream:

```yaml
requestBody:
  content:
    application/octet-stream: {}
```

For multipart uploads, retain `format: binary` until the target generator supports the desired 3.1 representation. Test generated clients before removing it.

### `$schema` dialect declaration

OAS 3.1 Schema Objects use the OAS base dialect by default:

```yaml
$schema: https://spec.openapis.org/oas/3.1/dialect/base
```

A referenced standalone JSON Schema can declare a different dialect. Add `$schema` when schemas are shared across OAS or JSON Schema versions and the toolchain supports the declared dialect. It is optional for ordinary schemas embedded in a single OAS 3.1 document.

## Useful new JSON Schema capabilities

OAS 3.1 supports JSON Schema 2020-12 features such as `if`/`then`/`else`, `prefixItems`, `dependentRequired`, `$defs`, and union types. Do not introduce them merely because of the migration: use a feature only when all intended validators, generators, and documentation tools support it.

## Checklist

- [ ] Target OAS 3.1 version and all tool versions are known.
- [ ] Every `nullable` conversion preserves the intended instances, including `$ref` and composition cases.
- [ ] Boolean exclusive bounds are converted correctly.
- [ ] Schema Object `example` usage is retained or modernized based on actual tooling support.
- [ ] File annotations reflect the wire representation and generated-client behavior.
- [ ] `$schema` dialects are declared only where sharing requires them.
- [ ] The resolved spec lints cleanly, the compatibility diff is reviewed, and contract tests pass.
