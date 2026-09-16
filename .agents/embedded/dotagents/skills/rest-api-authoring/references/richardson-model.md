# Richardson Maturity Model & Fielding's REST Constraints

## Richardson Maturity Model

Proposed by Leonard Richardson at QCon 2008 as a heuristic for judging how well a web service uses web technologies [crummy.com/2008/12/7](https://www.crummy.com/2008/12/7) → [martinfowler.com/articles/richardsonMaturityModel.html](https://martinfowler.com/articles/richardsonMaturityModel.html).

| Level | Name | Characteristics |
|-------|------|-----------------|
| 0 | Swamp of POX | Single endpoint, single method (usually POST), no HTTP verbs/headers/status codes. SOAP/XML-RPC live here. |
| 1 | Resources | Each aspect has its own URI, but only one HTTP method is used. Caching headers and status codes not leveraged. |
| 2 | HTTP Verbs | Resources addressed via URIs; different HTTP methods (GET, PUT, DELETE, POST) perform appropriate operations. Status codes convey outcomes. Amazon S3 is a cited Level 2 example. |
| 3 | Hypermedia Controls (HATEOAS) | Documents contain embedded hypermedia controls (links, forms) guiding clients to valid next actions. The Web itself, AtomPub, Netflix, and Launchpad are Level 3 examples. |

**Roy Fielding's position**: from the REST architectural style perspective, only Level 3 is truly REST — hypermedia is a **precondition**, not an optional topping. Without HATEOAS, an API cannot be called RESTful [roy.gbiv.com/untangled/2008/rest-apis-must-be-hypertext-driven](https://roy.gbiv.com/untangled/2008/rest-apis-must-be-hypertext-driven).

## Fielding's REST Constraints

Fielding's dissertation (*"Architectural Styles and the Design of Network-based Software Architectures"*, UC Irvine, 2000) defines REST as an architectural style for distributed hypermedia systems [ics.uci.edu](https://ics.uci.edu/~fielding/pubs/dissertation/abstract.htm).

- **Client-Server**: Separation of concerns — user interface separated from data storage, improving portability and independent evolution [§5.1.2](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.1.2).
- **Stateless**: Every request contains all information necessary to understand it; the server stores no client context between requests, improving visibility, reliability, and scalability [§5.1.3](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.1.3).
- **Cache**: Responses must be labeled as cacheable or non-cacheable, allowing reuse for later equivalent requests [§5.1.4](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.1.4).
- **Uniform Interface** (central differentiator):
  - *Identification of resources*: Resources identified by URIs [§5.2.1.1](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.2.1.1).
  - *Manipulation through representations*: Resource state captured and transferred via representations (bytes + metadata) in specific media types [§5.2.1.2](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.2.1.2).
  - *Self-descriptive messages*: Each message includes enough information to be understood independently [§5.2](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.2).
  - **HATEOAS**: Application state transitions driven exclusively by hypermedia provided dynamically in responses — the client selects from available transitions within received representations [§5.2](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.2).
- **Layered System**: Components interact only with their immediate layer, enabling intermediaries without changing component interfaces [§5.1.6](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.1.6).
- **Code-on-Demand** (optional): Client functionality extended by downloading executable code; the only optional REST constraint due to security trade-offs [§5.1.7](https://www.ics.uci.edu/~fielding/pubs/dissertation/rest_arch_style.htm#sec_5.1.7).

Fielding's 2008 blog post defines hypertext as *"the simultaneous presentation of information and controls such that the information becomes the affordance through which the user (or automaton) obtains choices and selects actions"* [roy.gbiv.com/untangled/2008/rest-apis-must-be-hypertext-driven](https://roy.gbiv.com/untangled/2008/rest-apis-must-be-hypertext-driven).

For the practical HATEOAS decision, media formats, runtime-discovery trade-offs, and adoption checklist, read `references/hateoas.md`.

## Web Linking (RFC 8288)

RFC 8288 obsoletes RFC 5988 and defines a generic framework for URI relationships usable in HTTP headers via the `Link` field. Registered relation types include `self`, `alternate`, `collection`, `item`, `next`, `prev`, `first`, `last`, and `describedby` [RFC 8288](https://www.rfc-editor.org/rfc/rfc8288.html).
