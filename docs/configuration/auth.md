# Auth

Quack authenticates **inbound** requests two ways, depending on how the client reaches it.

## Direct clients: OIDC

API, MCP, and A2A clients send a bearer token, verified against a configured OIDC issuer and
JWKS — any IdP works (Authentik, Keycloak, Auth0, …):

```yaml
auth:
  oidc:
    issuer: ${OIDC_ISSUER}
    audience: ${OIDC_AUDIENCE}
    jwks_url: ${OIDC_JWKS_URL}
```

- `issuer` — the IdP that issues and verifies tokens.
- `audience` — the expected token audience (e.g. `quack`).
- `jwks_url` — where to fetch the keys used to verify bearer tokens.

## Behind the gateway: trusted headers

Browser/SPA traffic doesn't re-run the OIDC flow against quack directly — it arrives through
a trusted forward-auth gateway, which injects identity as headers quack reads directly:

```yaml
auth:
  trusted_headers:
    user: X-authentik-username
    groups: X-authentik-groups
```

Either path — a verified bearer token or a trusted header — leaves the caller's identity
(user, groups) available for authorization.

## Gateway / deployment

The deployed instance runs behind a
[Traefik + Authentik gateway](https://github.com/fagerbergj/home-server/tree/main/api) on the
`api_gateway` network. Traefik routes `/api/v1/quack/*` to quack with the `authentik@file`
forward-auth middleware, which is what actually populates `X-authentik-*` — the public
`openapi.yaml` route is deliberately excluded from that middleware, since it needs no auth to
be readable.

The OpenAPI spec itself is rendered by the gateway's central `swagger-ui` container, not by
quack. Registering quack there means adding its spec URL to the `swagger-ui` `URLS` list in
`api/docker-compose.yml`, the same way `document-pipeline` is registered.
