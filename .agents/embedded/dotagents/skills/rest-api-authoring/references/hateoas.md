# HATEOAS: Decision Guide and Hypermedia Formats

HATEOAS, Hypertext As The Engine Of Application State, means a representation supplies the controls a client can use next. A client still needs an entry URI and understands the media type, but it discovers state-dependent transitions from received representations rather than constructing application-specific URLs from out-of-band endpoint knowledge.

Fielding treats hypermedia as a required REST constraint, not an optional feature: without it, an HTTP API may use REST-inspired practices but is not REST in the architectural sense [Fielding dissertation, §5.2](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.2) and [REST APIs must be hypertext-driven](https://roy.gbiv.com/untangled/2008/rest-apis-must-be-hypertext-driven). The Richardson Maturity Model calls this Level 3 [Martin Fowler](https://martinfowler.com/articles/richardsonMaturityModel.html).

## Decision rule

Use hypermedia when the controls available to a client genuinely vary with resource state, permissions, workflow stage, or server capability, and clients can act on those controls. Examples include an order that can be cancelled only before fulfillment, a workflow task that exposes only permitted transitions, or a public API whose resource topology can evolve independently of clients.

Do not add links merely to call an API RESTful. Prefer conventional endpoint documentation and stable, direct calls when the clients are closed and deployed together, the workflow is known at build time, none of the offered transitions vary by state, or the added representation and client-handling complexity has no demonstrated consumer. This is a product and coupling decision, not a maturity score to optimize.

Before adopting it, answer:

- Which state-dependent controls will clients actually follow?
- Which standard link relation types express them, and how will custom relation types be documented?
- Does the chosen media type describe only navigation, or does it also describe forms/actions?
- Can the generated clients, gateway, cache, and documentation renderer preserve the media type and links?
- What latency and payload budget is acceptable if clients follow more than one link?

## What to put in a representation

A useful hypermedia response has:

- a canonical `self` link;
- links to related resources with registered relation types where possible;
- custom relation URIs with stable documentation where no registered relation fits;
- only controls that are currently valid for the authenticated caller and resource state;
- forms or action descriptions when the client must discover request method, target, fields, and constraints at runtime.

`Link` headers and relation types are standardized by [RFC 8288](https://www.rfc-editor.org/rfc/rfc8288.html), which obsoletes RFC 5988. Hypermedia controls do not replace authorization checks: a server must still enforce every control when it is invoked.

## Format selection

| Format | Best for | Includes | Trade-off |
|---|---|---|---|
| **HAL** (`application/hal+json`) | Simple resource navigation | `_links`, `_embedded`, URI templates | Minimal and widely supported, but it does not standardize write forms/actions. |
| **Collection+JSON** (`application/vnd.collection+json`) | CRUD-oriented collections | items, links, query templates, write templates | Useful collection conventions; less suitable for complex, non-collection workflows. |
| **Siren** (`application/vnd.siren+json`) | State-machine or task-oriented clients | links, entities, first-class actions and fields | Describes actionable controls well; fewer tooling integrations than HAL. |
| **Hydra** (JSON-LD/RDF vocabulary) | Linked-data and semantic-web APIs | operations, supported properties, IRI templates | Strong semantic model; high complexity for ordinary JSON consumers. |

HAL's small surface makes it a good starting point when links alone solve the problem. Choose Siren or Collection+JSON only when a runtime client needs standardized write affordances. Choose Hydra only when RDF/JSON-LD interoperability is a real requirement.

Sources: [HAL Internet-Draft](https://datatracker.ietf.org/doc/html/draft-kelly-json-hal-11), [HAL specification](https://stateless.co/hal_specification.html), [Collection+JSON](https://github.com/collection-json/spec), [Siren](https://github.com/kevinswiber/siren), and [Hydra Core Vocabulary](https://www.hydra-cg.com/spec/latest/core/).

## GraphQL introspection is not runtime hypermedia

GraphQL introspection exposes the schema's possible types, fields, queries, and mutations. It improves tooling and schema discovery, but it does not generally state which transition is valid for this particular resource, user, and current state. Hypermedia controls can do that at runtime.

The systems can coexist, but optimize different things:

| Concern | Hypermedia | GraphQL introspection |
|---|---|---|
| Discovery | State-specific controls in a representation | Global schema capabilities |
| Client model | Follow available controls at runtime | Query known types and fields |
| Caching | HTTP representations and links can use ordinary HTTP caching | Requires GraphQL-aware caching policy |
| Evolution | Add relations or controls, preserving old ones | Add/deprecate schema fields and operations |

Do not choose hypermedia only for self-documenting tooling: a well-described OpenAPI or GraphQL schema can solve that need. Choose it for runtime affordances and looser coupling to resource topology.

## Published examples, not adoption proof

NRK described using HAL links to compose bounded contexts across its TV microservices, while noting HAL's lack of form descriptions as a drawback [NRK](https://nrkbeta.no/2018/02/08/on-architecture-third-post-composing-bounded-contexts/). Spring HATEOAS and Spring Data REST provide mature Java support for HAL-style APIs [Spring HATEOAS](https://docs.spring.io/spring-hateoas/docs/current/reference/html/). These show that the approach is viable; they do not establish that it is the right default for another API.

## Validation checklist

- [ ] Every emitted control is valid for the current resource state and authenticated caller.
- [ ] Each standard relation uses its registered meaning; every custom relation has stable documentation.
- [ ] A client can perform at least one meaningful workflow without constructing a hidden application URL.
- [ ] The selected format represents all required controls, including forms/actions when needed.
- [ ] Direct endpoint calls remain authorized and safe even if a client ignores hypermedia.
- [ ] Contract tests cover both a permitted and unavailable transition.
- [ ] Payload size, follow-up request count, caching, and generated-client support are measured in the target deployment.
