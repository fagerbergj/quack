# OWASP API Security Review

The authoritative source is the [OWASP API Security Project](https://owasp.org/www-project-api-security/) and its [2023 Top 10](https://owasp.org/API-Security/editions/2023/en/0x11-t10/).

| # | Category |
|---|----------|
| API1:2023 | Broken Object Level Authorization |
| API2:2023 | Broken Authentication |
| API3:2023 | Broken Object Property Level Authorization |
| API4:2023 | Unrestricted Resource Consumption |
| API5:2023 | Broken Function Level Authorization |
| API6:2023 | Unrestricted Access to Sensitive Business Flows |
| API7:2023 | Server Side Request Forgery |
| API8:2023 | Security Misconfiguration |
| API9:2023 | Improper Inventory Management |
| API10:2023 | Unsafe Consumption of APIs |

## Per-operation review record

For every published operation, record the following before approval:

- **Identity and authorization:** authentication method, required scope/role, tenant boundary, and an object-level authorization test for an ID owned by another subject.
- **Input and output fields:** an allowlist of writable properties, any field-level authorization, and confirmation that sensitive response fields are omitted or masked for each role.
- **Resource limits:** pagination limits, request-size limit, rate-limit policy, and any expensive filter, sort, upload, or asynchronous-work limit.
- **Business flow:** abuse controls for high-value actions such as signup, checkout, password recovery, invitation, or bulk export.
- **Outbound calls:** URL allowlist or egress policy, DNS/IP validation, redirect policy, callback/webhook signature verification, timeout, and response-data validation.
- **Errors and observability:** Problem Details fields that are safe to expose, audit events, correlation IDs, and no tokens, secrets, stack traces, or authorization decisions in responses.
- **Inventory:** owner, audience, environment, published version, deprecation/sunset status, and retirement date where relevant.

## Evidence to require

An OpenAPI `security` requirement documents authentication expectations; it does not prove object-, property-, or function-level authorization. Require implementation tests or policy evidence for those controls. Record exceptions with an owner, risk acceptance, and expiry date.

## Endpoint prompts

- **Read/write/delete by ID:** test the same operation against another tenant or user (API1) and an unauthorized role (API5).
- **Create or update:** attempt forbidden properties such as `role`, `ownerId`, price, state, and internal flags (API3).
- **List/search/export:** test large pages, broad filters, repeated requests, and expensive sorts (API4).
- **Authentication and recovery:** test token rotation, expiry, revocation, audience, and rate limits (API2, API6).
- **Webhook, URL, or import endpoints:** test private-address, redirect, malformed, and untrusted-upstream inputs (API7, API10).
- **Administrative or legacy routes:** verify they are inventoried, authenticated, and retired when unused (API8, API9).
