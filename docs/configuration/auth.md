# Auth

Quack's inbound request auth (`internal/auth`) gates the API surface - the generated REST routes and the MCP mount (`/api/v1/mcp`) - but never `/health` or the embedded SPA's static files. The `auth:` config section is entirely optional: absent, as it is by default, nothing is enforced and every request is open. That matches the state of the codebase before this feature existed, so an unconfigured deployment sees no behavior change.

```yaml
auth:
  oidc:
    issuer: ${OIDC_ISSUER}
    audience: ${OIDC_AUDIENCE}
    jwks_url: ${OIDC_JWKS_URL} # optional override; see below
  trusted_headers:
    user: X-authentik-username
    groups: X-authentik-groups
```

Present, `auth:` needs at least one of the two sub-blocks below - `config.Load` rejects a present-but-empty `auth:` section at startup. Configuring only one is normal; both together is also valid (see precedence).

## Direct clients: OIDC bearer tokens

`auth.oidc` verifies a bearer token against any OIDC-compliant issuer (Authentik, Keycloak, Auth0, …):

- `issuer` and `audience` are required - a config with `oidc:` present but either empty fails to load.
- On startup, `internal/auth.New` fetches `<issuer>/.well-known/openid-configuration` and reads its `jwks_uri` - **discovery is the default path**. This happens once, synchronously: an unreachable or malformed issuer is a startup error, not a silent fallback to open auth.
- `jwks_url` is an optional override that skips discovery entirely when set - use it only when discovery itself is unavailable or network-blocked.
- Signing keys are fetched and cached by [`github.com/MicahParks/keyfunc/v3`](https://github.com/MicahParks/keyfunc), which refreshes the JWKS in the background and selects the verification key by the token's `kid`. Verification itself (`golang-jwt/jwt/v5`, already a dependency for the GitHub App's own JWT) checks signature, `iss`, `aud`, and `exp`.
- The caller identity is read from the token's claims: `preferred_username` (falling back to `sub`) for the user, and an optional `groups` claim.

A request with no bearer token, an expired one, a bad signature, or a mismatched issuer/audience gets `401 Unauthorized`.

## Behind the gateway: trusted headers

`auth.trusted_headers` names the headers a forward-auth gateway injects after authenticating the request itself - the SPA's own traffic doesn't re-run the OIDC flow against quack directly, it arrives through something like a Traefik + Authentik forward-auth middleware:

- `user` is required; `groups` is optional.
- When the named user header is present on a request, quack trusts it outright and does **not** attempt bearer-token verification, even if `oidc` is also configured.

This precedence - trusted headers win when present - is deliberate: it's the gateway-fronted path, and the gateway has already done the real authentication work.

## Neither configured, one configured, both configured

| `auth:` section | Request has trusted header | Request has bearer token | Result |
|---|---|---|---|
| absent | - | - | open, no enforcement |
| `trusted_headers` only | yes | - | trusted, identity from header |
| `trusted_headers` only | no | - | `401` |
| `oidc` only | - | valid | verified, identity from claims |
| `oidc` only | - | missing/invalid | `401` |
| both | yes | (ignored) | trusted, identity from header |
| both | no | valid | verified, identity from claims |
| both | no | missing/invalid | `401` |

The verified identity (`auth.Identity{User, Groups}`) is attached to the request context (`auth.FromContext`) either way, for handlers that want to authorize on it.

## Implementation

- `internal/config/config.go` - `InboundAuthConfig`/`OIDCConfig`/`TrustedHeadersConfig` and their `config.Load`-time validation.
- `internal/auth/auth.go` - the `*Auth` type, its chi middleware, and the trusted-headers-priority logic. A `nil *Auth` (config absent) is a no-op passthrough.
- `internal/auth/oidc.go` - discovery, JWKS, and token verification.
- `internal/server/router.go` - mounts the middleware on a chi `Group` scoped to the MCP mount and the generated REST routes, explicitly excluding `/health`; extension webhook routes (e.g. the GitHub App's, verified by their own HMAC signature) and the SPA sit outside that group entirely.

## Gateway / deployment

The deployed instance runs behind a [Traefik + Authentik gateway](https://github.com/fagerbergj/home-server/tree/main/api) on the `api_gateway` network, using the `authentik@file` forward-auth middleware to populate `X-authentik-*`.

Publishing `openapi.yaml` as a readable, unauthenticated route via the gateway's central `swagger-ui` container is **not yet implemented** - no such route exists in this codebase today.
