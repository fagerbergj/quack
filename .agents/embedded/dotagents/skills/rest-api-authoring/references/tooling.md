# Validation & Testing Tooling

## Spectral (stoplightio/spectral)

A flexible JSON/YAML linter with baked-in support for OpenAPI v3.1, v3.0, v2.0, AsyncAPI v2.x, and Arazzo v1 [github.com/stoplightio/spectral](https://github.com/stoplightio/spectral) → [docs.stoplight.io/docs/spectral/e5b9616d6d50c-rulesets](https://docs.stoplight.io/docs/spectral/e5b9616d6d50c-rulesets).

- Comes with a built-in `spectral:oas` ruleset (extends it via `extends: "spectral:oas"`) covering OAS 2/3 validation rules auto-detected from the spec version.
- Rulesets are written in JSON/YAML or JavaScript and configured via `.spectral.yaml`.
- OpenAPI rules reference: [github.com/stoplightio/spectral/blob/develop/docs/reference/openapi-rules.md](https://github.com/stoplightio/spectral/blob/develop/docs/reference/openapi-rules.md).

**Usage**: `spectral lint openapi.yaml` — fix all errors; treat warnings as actionable unless documented as intentional.

## oasdiff (tufin/oasdiff)

A command-line tool and Go library for comparing OpenAPI 3.x specs and detecting breaking changes — a *diff engine* focused on schema evolution [specdrift.dev/guides/openapi-breaking-changes](https://specdrift.dev/guides/openapi-breaking-changes).

- Detects **509 distinct API changes** across 10 OpenAPI areas (paths, components, schema, parameters, request bodies, responses, headers, security, tags, info), each classified as breaking/warning/informational [oasdiff.com/docs/breaking-changes](https://www.oasdiff.com/docs/breaking-changes).
- Three commands: `diff` (full machine-readable diff), `changelog` (human-readable), and `breaking` (breaking-only, ideal for CI gates).

**CI gate**: `oasdiff breaking old.yaml new.yaml --exit-code 1` — fails the build on any accidental breaking change.
- GitHub Action: [oasdiff/oasdiff-action](https://github.com/oasdiff/oasdiff-action)

## openapi-lint (oxidecomputer/openapi-lint)

A Rust crate by Oxide Computer Company focused on ergonomic OpenAPI v3.0.3 validation — specifically flagging constructs that make SDK generators produce poor code [github.com/oxidecomputer/openapi-lint](https://github.com/oxidecomputer/openapi-lint). Not a general-purpose linter; targets SDK-generation readiness (naming conventions, UUID validation, external rules for "external" APIs).

## Schemathesis — Contract Testing

A property-based testing framework for OpenAPI and GraphQL APIs. It automatically generates thousands of test cases from a schema, exercising edge cases that manual tests miss [schemathesis.readthedocs.io/en/stable](https://schemathesis.readthedocs.io/en/stable/).

Key features:
- Generates property-based inputs for each operation defined in your schema.
- Runs core checks: status-code conformance, response validation against the OpenAPI spec, and server-error detection (catches 5xx responses from inputs that should be rejected with 4xx).
- Reports reproduction-ready `curl` commands for any failing test case.
- Supports OpenAPI 2.0, 3.0, 3.1, 3.2, and GraphQL.
- CLI (`schemathesis run <schema-url>`), Python library, or via Docker — no permanent Python install required when using `uvx`.
- Integrates with pytest, exports results as JUnit XML/HAR/VCR cassettes for CI/CD pipelines.

**Quick example**:
```bash
uvx schemathesis run https://example.schemathesis.io/openapi.json
```

This generates diverse inputs targeting edge cases like `{"number": "\n\uudbcd."}` to trigger type-handling bugs in the server [schemathesis.readthedocs.io/en/latest/quick-start](https://schemathesis.readthedocs.io/en/latest/quick-start/).

## Typical Validation Pipeline

1. **Pre-commit**: `spectral lint` — catches spec syntax errors early.
2. **CI on PR**: `oasdiff breaking old.yaml new.yaml --exit-code 1` — fails the job when there are unintended breaking changes.
3. **Before publishing**: `schemathesis run <mock-server-url>` — validates the deployed implementation matches the spec.
