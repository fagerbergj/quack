# API Versioning Strategies

Versioning is a contract and deployment decision, not a REST compliance test. First follow an existing public API convention. For a new API, choose the strategy that your consumers, gateway, cache, documentation, and retirement process can support.

## Selection criteria

Check these before choosing a syntax:

- Can consumers identify the version easily in requests, logs, examples, and support tickets?
- Can the gateway route the version and can the CDN include the relevant request part in its cache key?
- If representations vary by `Accept`, is `Vary: Accept` sent and honored by every intermediary?
- Can generated SDKs and interactive documentation express the strategy cleanly?
- How will consumers discover versions, receive deprecation notices, and migrate?

## Common strategies

| Strategy | Strengths | Costs and checks |
|----------|-----------|------------------|
| **URI path** (`/v1/users`) | Visible in URLs, logs, and `curl`; straightforward routing and usually easy cache separation. | Treats versions as distinct route spaces. Confirm route policy and whether a versioned path matches existing API conventions. |
| **Media type / Accept header** (`Accept: application/vnd.example.v1+json`) | Keeps the resource URI stable and selects a representation through content negotiation. | Configure `Vary: Accept`, edge cache keys, routing, observability, and tooling. Version discovery is less obvious in copied URLs. |
| **Query parameter** (`?version=1`) | Easy to add during a migration and visible in a request. | Confirm the CDN/proxy cache key includes the query parameter and that it does not conflict with application query semantics. |
| **Date-based version** (`2026-04-22`) | Makes the selected contract explicit and can support per-account upgrade behavior. | Requires clear release documentation, client defaults, and a durable compatibility policy. |

URI-path versioning is a practical default for a new public API when there is no established convention and the deployment has ordinary path-based routing. It is not universally better: a correctly configured cache can key on query parameters or `Accept`, while a poorly configured path cache can still serve the wrong response.

Sources: [REST API versioning strategies](https://www.application-architect.com/posts/rest-api-versioning-url-header-and-query-parameter-strategies/), [Vercel API versioning strategies](https://vercel.com/i/api-versioning-strategies), [RFC 9111 caching](https://www.rfc-editor.org/rfc/rfc9111.html).
