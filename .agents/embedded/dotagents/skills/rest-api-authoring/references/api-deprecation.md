# API Deprecation Standards

Two IETF standards govern API deprecation signaling at the HTTP level:

## Sunset Header — RFC 8594 (Informational, May 2019)

Defines a `Sunset` response header indicating that a URI will likely become unresponsive at a specified future date/time [datatracker.ietf.org/rfc8594](https://datatracker.ietf.org/doc/rfc8594/). Also defines a `sunset` link relation type for linking to resources about migration or sunset policies.

**Status**: Informational (not Standards Track), authored by Erik Wilde for the IETF.

## Deprecation Header — RFC 9745 (Proposed Standard, March 2025)

Defines a `Deprecation` response header field used to signal to consumers of a URI that the resource will be or has been deprecated [datatracker.ietf.org/rfc9745](https://datatracker.ietf.org/doc/rfc9745/). Additionally defines a `deprecation` link relation type to point clients at migration documentation.

**Status**: **Standards Track**, authored by Sanjay Dalal and Erik Wilde, published by the httpapi WG. Deprecation does *not* change the resource's runtime behavior — it purely informs clients so they can migrate proactively.

## How They Interact

The two headers interact naturally: `Deprecation` says "we're deprecating this," `Sunset` says "and this will stop working on date X" [datatracker.ietf.org/rfc9745](https://datatracker.ietf.org/doc/rfc9745/) → [www.rfc-editor.org/info/rfc9745](https://www.rfc-editor.org/info/rfc9745).

Use both together for a complete deprecation lifecycle: `Deprecation` header with current timestamp, followed by `Sunset` with an expiration date. Include `Link: <migration-docs>; rel="deprecation"` to point clients at migration guidance.
