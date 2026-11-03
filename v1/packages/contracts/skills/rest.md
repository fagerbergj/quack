# REST contract guide

Sources: RFC 9110 and 9457, OpenAPI 3.1, Zalando RESTful API Guidelines, Google AIP-180, Azure REST guidelines, OWASP API Security Top 10 (2023). A rule marked check is enforced mechanically; a rule marked ask is a choice for the person, with a suggested default and its trade-off.

## Intent first

Settle these before drafting: purpose and non-goals, audience (internal or external), consumers and how they couple to the contract (generated SDKs, gateway, cache keys), guarantees (compatibility, idempotency, consistency, limits, authorization), deprecation terms. Read the house convention in the code first: an existing OpenAPI file, gateway routing, error shape, auth. A convention you find beats any default below.

### document
Check. OpenAPI 3.1.x, self-contained: references stay inside the document, none remote.

### methods
Check. Follow RFC 9110 semantics. GET, HEAD and DELETE have no request body. GET, HEAD, OPTIONS are safe; PUT and DELETE are idempotent; POST and PATCH are not. After DELETE, GET returns 404 or 410. A query too large for a URL uses a documented POST.

### status-codes
Check. Only registered status codes with their RFC meaning, the most specific one. Declare every success response and every endpoint-specific error; generic 401, 403, 404, 500, 503 may fall to `default`.

### errors
Check. Errors are `application/problem+json` (RFC 9457) unless the house convention says otherwise. Never expose stack traces.

### payload
Check. A JSON object at the top level, never a bare array. Treat objects as open for extension; clients ignore unknown fields. Use `id` as an opaque string. Dates are RFC 3339.

### compatibility
Check. These break callers: removing or renaming a field (optional ones too), adding a required request field, changing any type (widening included, since generated code changes), changing a default, narrowing an enum or loosening it in a response without extensible semantics, tightening validation, renaming a resource, changing auth, adding paging later. Adding optional response fields, new operations and loosened input validation are safe. A breaking change names its callers.

### deprecation
Check. `deprecated: true` plus a description saying where callers go. Signal with `Deprecation` and `Sunset` headers. Watch usage before sunset; get consent from known callers first.

### security
Check. Document auth with a `security` block. It records authentication and proves nothing about authorization: per operation, note who may call, which object boundary applies, which fields are writable, and the limits (OWASP API1 to API10).

### versioning
Ask. First the house convention; an existing unversioned route keeps its shape. Otherwise suggest Google's model (AIP-185): a major version only, as the first path segment (`/v1`); compatible changes ship in place; a new major only for a breaking change; `v1alpha` and `v1beta` for unstable channels. Alternatives, with their trade-offs: a query parameter (Azure: easy for gateways and caches to ignore, but versions leak into every cache key) or a media type (Zalando: keeps URLs stable, but is harder for SDKs, CDNs and browsers).

### naming
Ask. Property case (snake or camel, one only), enum value case, whether enums are extensible. Resource names are plural nouns in kebab-case with no verbs.

### paging
Ask. Cursor or offset, and whether totals are returned. Cursor is stable under inserts but cannot jump to a page. Adding paging later breaks callers, so decide it up front for any list that can grow.

### idempotency
Ask for POST and PATCH that callers retry: a conditional key (If-Match), a secondary unique key, or an Idempotency-Key header. Prefer the first two.

### long-running
Ask when work outlasts a request: 202 with a status monitor, or a job resource.

### validation-errors
Ask. 422 or 400 for validation failures, and whether request schemas are closed. RFC 9110 defines 422; the Zalando guide prefers 400. Pick one.

### richardson
Ask only if links really vary with state. Otherwise stay at level 2: resources and HTTP verbs.
