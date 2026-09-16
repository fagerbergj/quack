# Requirement Diagram — examples

## 1. Authentication system traceability

What this shows: functional and interface requirements for a login system, traced to the UI and service elements that satisfy them.

```mermaid
requirementDiagram

requirement login_req {
    id: "AUTH-1"
    text: Users must authenticate with email and password
    risk: high
    verifymethod: test
}

functionalRequirement mfa_req {
    id: "AUTH-2"
    text: "Users must be able to enable multi-factor authentication"
    risk: medium
    verifymethod: demonstration
}

interfaceRequirement api_req {
    id: "AUTH-3"
    text: Auth service must expose a REST token endpoint
    risk: low
    verifymethod: inspection
}

element login_ui {
    type: React component
}

element auth_service {
    type: Go service
    docref: docs/auth.md
}

login_ui - satisfies -> login_req
auth_service - satisfies -> api_req
login_req - contains -> mfa_req
mfa_req - traces -> api_req
```

## 2. API rate limiting, left-to-right

What this shows: a performance requirement and the design constraint it derives, rendered `LR` for a shorter/wider layout.

```mermaid
requirementDiagram

direction LR

performanceRequirement rate_limit {
    id: "API-10"
    text: API must reject more than 100 requests per minute per client
    risk: medium
    verifymethod: test
}

designConstraint token_bucket {
    id: "API-11"
    text: Rate limiting must use a token bucket algorithm
    risk: low
    verifymethod: analysis
}

element gateway {
    type: Go middleware
    docref: internal/server/ratelimit
}

gateway - satisfies -> rate_limit
rate_limit - derives -> token_bucket
```

## 3. Safety-critical medical device requirements

What this shows: high-risk physical and design requirements with multiple verification methods, plus a highlighted-class for the critical path.

```mermaid
requirementDiagram

requirement patient_safety {
    id: "MED-1"
    text: "Device must not deliver a dose exceeding the prescribed limit"
    risk: high
    verifymethod: test
}

physicalRequirement dose_hw_limit {
    id: "MED-2"
    text: "Pump hardware must enforce a hard maximum flow rate"
    risk: high
    verifymethod: analysis
}

designConstraint fda_class2 {
    id: "MED-3"
    text: "Design must comply with FDA Class II device requirements"
    risk: medium
    verifymethod: inspection
}

element firmware {
    type: embedded C
    docref: firmware/pump_control
}

element hardware_review {
    type: design review
    docref: reviews/pump_hw_2026
}

firmware - satisfies -> patient_safety
patient_safety - derives -> dose_hw_limit
hardware_review - verifies -> dose_hw_limit
patient_safety - traces -> fda_class2

classDef critical fill:#f96,stroke:#900,stroke-width:2px
class patient_safety,dose_hw_limit critical
```
