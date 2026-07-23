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
- On startup, `internal/auth.New` fetches `<issuer>/.well-known/openid-configuration` (via [`github.com/zitadel/oidc/v3`](https://github.com/zitadel/oidc)'s `client.Discover`) and reads its `jwks_uri` - **discovery is the default path**. Discovery also rejects a response whose own `issuer` field doesn't match the configured one. This happens once, synchronously, followed by one reachability probe of the JWKS itself: an unreachable or malformed issuer or JWKS is a startup error, not a silent fallback to open auth.
- `jwks_url` is an optional override that skips discovery entirely when set - use it only when discovery itself is unavailable or network-blocked. It still means exactly what it did before: the JWKS endpoint quack fetches signing keys from.
- Signing-key resolution and caching is `rp.NewRemoteKeySet` (zitadel/oidc's relying-party package, used here purely as a JWKS-backed `oidc.KeySet` - no browser/redirect flow, no `RelyingParty` object beyond the verifier itself), which refreshes the JWKS lazily and selects the verification key by the token's `kid`. Verification (`rp.VerifyIDToken` against an `*oidc.IDTokenClaims`) checks signature, `iss`, `aud`, `exp`, and `iat`.
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

- `internal/config/config.go` - `InboundAuthConfig`/`OIDCConfig`/`TrustedHeadersConfig` and their `config.Load`-time validation. Unchanged by the zitadel/oidc rebuild - same YAML fields, same validation, same precedence.
- `internal/auth/auth.go` - the `*Auth` type, its chi middleware, and the trusted-headers-priority logic. A `nil *Auth` (config absent) is a no-op passthrough.
- `internal/auth/oidc.go` - discovery, JWKS, and token verification, built on `github.com/zitadel/oidc/v3`'s `pkg/client` (discovery), `pkg/client/rp` (`NewIDTokenVerifier` + `NewRemoteKeySet` - the JWKS-verifier primitives, not the browser-flow `RelyingParty`), and `pkg/oidc` (`IDTokenClaims`, the verification checks these two build on).
- `internal/server/router.go` - mounts the middleware on a chi `Group` scoped to the MCP mount and the generated REST routes, explicitly excluding `/health`; extension webhook routes (e.g. the GitHub App's, verified by their own HMAC signature) and the SPA sit outside that group entirely.

### Why zitadel/oidc over golang-jwt/jwt + MicahParks/keyfunc

The original implementation hand-rolled the discovery fetch and used `golang-jwt/jwt/v5` + `MicahParks/keyfunc/v3` for JWKS-by-`kid` resolution and verification. `github.com/zitadel/oidc/v3` replaces both: `pkg/client.Discover` for discovery (and it validates the discovery document's own `issuer`, which the hand-rolled fetch didn't), and `pkg/client/rp`'s `NewIDTokenVerifier`/`NewRemoteKeySet`/`VerifyIDToken` for everything from there - signature, `iss`, `aud`, `exp`, `iat`. Despite living in the `rp` (relying-party) package, these are self-contained primitives that need only issuer + audience + a JWKS source; they don't require constructing a full `RelyingParty` or running any part of the browser/redirect authorization-code flow, which is exactly the fit a bearer-token-verifying resource server needs. `golang-jwt/jwt/v5` stays a dependency (the GitHub App's own JWT signing in `internal/github/app.go` is unrelated); `MicahParks/keyfunc` and `MicahParks/jwkset` are gone.

One deliberate addition on top of the library: `rp.NewRemoteKeySet` fetches the JWKS lazily on first use, whereas the original fetched it eagerly at boot so a bad `jwks_url` failed server startup rather than surfacing as a run of 401s. `internal/auth/oidc.go` restores that by probing the JWKS during `auth.New`, retrying a couple of times with backoff so a transient blip (e.g. mid rolling-deploy) doesn't fail startup outright - it still fails once retries are exhausted, and each probed key must carry `kty` plus `kid`/`alg`, so a structurally broken JWKS response fails loudly instead of only breaking the first real request (result otherwise discarded - the real, cached fetch remains `rp.NewRemoteKeySet`'s own).

## CLI login (`quack server login`)

The CLI is also a zitadel/oidc client - but as an actual relying party this time, since it's a real user logging in against a browser-facing IdP. `quack server login <name> --issuer <url> --client-id <id>` runs the OAuth 2.0 **authorization code flow with PKCE** (RFC 6749 + RFC 7636), the RFC 8252 native-app pattern, via `pkg/client/rp`'s `NewRelyingPartyOIDC`, `AuthURL`, and `CodeExchange`: it binds a loopback listener on an ephemeral port to stand in for the redirect URI, opens the authorize URL in a browser (also printed, as a fallback), and waits for the redirect to land back on the listener before exchanging the code (PKCE-verified) for an access + refresh token pair.

This replaced an earlier device authorization grant (RFC 8628) implementation. Auth code + PKCE is the flow RFC 8252 actually recommends for a CLI/native client, but it trades away the device grant's one real advantage: it needs a browser and a port reachable from that browser on the machine running `quack`, so it does **not** work over a headless/SSH session with no local browser - the device grant's actual use case. Only public OIDC clients are supported (no client secret): PKCE is meant for exactly that client shape, and it means the CLI never has a secret to protect.

Tokens are stored per registered server in the same registry `quack server add` already writes to (`~/.quack/servers.yaml`, `$QUACK_HOME`): issuer, client ID, granted scopes, the token endpoint URL (cached from discovery so a later refresh needs no re-discovery), and the access/refresh tokens with the access token's expiry. That file (and its directory) is always written `0600`/`0700`, since it can hold live credentials once any server has logged in. `quack chat`/`quack api`/`-p` attach the stored access token as `Authorization: Bearer <token>` automatically for a server resolved from the registry (active server, or a `--server` value that matches a registered URL), refreshing it first via the token endpoint's `refresh_token` grant (`golang.org/x/oauth2`'s `Config.TokenSource`) whenever it's within 30 seconds of expiry - a mutex coalesces concurrent refreshes of the same server within one process (e.g. the TUI firing off more than one client call at once) into a single request. A server with no stored session behaves exactly as before - no header is attached.

## Gateway / deployment

The deployed instance runs behind a [Traefik + Authentik gateway](https://github.com/fagerbergj/home-server/tree/main/api) on the `api_gateway` network, using the `authentik@file` forward-auth middleware to populate `X-authentik-*`.

Publishing `openapi.yaml` as a readable, unauthenticated route via the gateway's central `swagger-ui` container is **not yet implemented** - no such route exists in this codebase today.
