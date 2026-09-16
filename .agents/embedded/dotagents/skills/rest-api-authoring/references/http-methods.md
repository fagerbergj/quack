# HTTP Method Semantics & Status Codes

## HTTP Methods

The primary specification is **RFC 9110** (*HTTP Semantics*), published in 2022 and obsoleting RFC 7231 [datatracker.ietf.org/rfc9110](https://datatracker.ietf.org/doc/html/rfc9110). Method definitions remain at Sections 9.3.1–9.3.5 with the same semantics.

| Method | Safe? | Idempotent? | Description |
|--------|-------|-------------|-------------|
| **GET** | ✅ Yes | ✅ Yes | Transfer a current representation of the target resource [RFC 9110 §9.3.1](https://datatracker.ietf.org/doc/html/rfc9110#name-get). |
| **POST** | ❌ No | ❌ No | Perform resource-specific processing on the request payload (e.g., creating a subordinate resource) [RFC 9110 §9.3.3](https://datatracker.ietf.org/doc/html/rfc9110#name-post). |
| **PUT** | ❌ No | ✅ Yes | Replace all current representations with the request payload [RFC 9110 §9.3.4](https://datatracker.ietf.org/doc/html/rfc9110#name-put). |
| **DELETE** | ❌ No | ✅ Yes | Remove all current representations of the target resource [RFC 9110 §9.3.5](https://datatracker.ietf.org/doc/html/rfc9110#name-delete). |
| **PATCH** | ❌ No | Not guaranteed | Applies partial modifications; not idempotent because the same patch applied twice may produce different results depending on intervening changes [RFC 5789](https://datatracker.ietf.org/doc/html/rfc5789). |

## Status Code Families

Defined in RFC 9110 Section 15 (obsoletes RFC 7231 Section 6), with additions from RFC 6585 [datatracker.ietf.org/rfc9110](https://datatracker.ietf.org/doc/html/rfc9110) → [datatracker.ietf.org/rfc6585](https://datatracker.ietf.org/doc/html/rfc6585).

| Range | Category | Examples |
|-------|----------|----------|
| **1xx** | Informational | 100 Continue, 101 Switching Protocols |
| **2xx** | Success | 200 OK, 201 Created, 202 Accepted, 204 No Content |
| **3xx** | Redirection | 301 Moved Permanently, 302 Found, 303 See Other |
| **4xx** | Client Error | 400 Bad Request, 401 Unauthorized, 403 Forbidden, 404 Not Found, 405 Method Not Allowed, 409 Conflict, 415 Unsupported Media Type, 422 Unprocessable Content, 428 Precondition Required, 429 Too Many Requests, 431 Request Header Fields Too Large |
| **5xx** | Server Error | 500 Internal Server Error, 502 Bad Gateway, 503 Service Unavailable, 504 Gateway Timeout, 511 Network Authentication Required |

## Problem Details (RFC 7807 / RFC 9457)

RFC 7807 defines a standard machine-readable format for error details in HTTP responses, avoiding per-API error format definition [datatracker.ietf.org/rfc7807](https://datatracker.ietf.org/doc/html/rfc7807). The `ProblemDetails` JSON object supports `type` (URI reference), `title`, `status` (HTTP status code), `detail`, and `instance`.

RFC 9457 obsoletes RFC 7807 (July 2023). It permits problem-type definitions to add extension members directly to the object; there is no `extensionsMap` field. Clients must ignore extensions they do not recognize [RFC 9457 §3.2](https://datatracker.ietf.org/doc/html/rfc9457#section-3.2). Use this format for API error responses to give clients predictable error structures across endpoints.
