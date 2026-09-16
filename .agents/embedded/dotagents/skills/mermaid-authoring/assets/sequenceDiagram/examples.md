# Sequence diagram examples

## Login flow with activation

What this shows: activation bars via the `+`/`-` shortcut and an `alt` branch for bad credentials.

```mermaid
sequenceDiagram
    participant Client
    participant API
    participant DB
    Client->>+API: POST /login
    API->>+DB: Query user by email
    DB-->>-API: User record
    alt valid password
        API-->>Client: 200 OK + token
    else invalid password
        API-->>-Client: 401 Unauthorized
    end
```

## CI pipeline handoff between services

What this shows: async fire-and-forget messages (`-)`) and a note spanning two participants.

```mermaid
sequenceDiagram
    participant Dev
    participant CI
    participant Registry
    participant CD
    Dev-)CI: push commit
    CI->>CI: run tests
    Note over CI,Registry: Only on green build
    CI-)Registry: push image
    Registry-)CD: image available
    CD->>CD: deploy
```

## Parallel notification fan-out

What this shows: `par`/`and` for concurrent branches converging back on the caller.

```mermaid
sequenceDiagram
    participant Service
    participant Email
    participant SMS
    participant Slack
    par notify by email
        Service->>Email: send
    and notify by SMS
        Service->>SMS: send
    and notify by Slack
        Service->>Slack: send
    end
    Email-->>Service: sent
    SMS-->>Service: sent
    Slack-->>Service: sent
```

## DB connection with critical/option error handling

What this shows: `critical`/`option` for an action that must run with conditional failure handling.

```mermaid
sequenceDiagram
    participant Service
    participant DB
    critical Establish a connection to the DB
        Service-->DB: connect
    option Network timeout
        Service-->Service: log timeout error
    option Credentials rejected
        Service-->Service: log auth error
    end
```

## Order booking with a break on failure

What this shows: `break` to short-circuit the sequence when a step fails.

```mermaid
sequenceDiagram
    participant Consumer
    participant API
    participant BookingService
    participant BillingService
    Consumer-->API: Book something
    API-->BookingService: Start booking process
    break when the booking process fails
        API-->Consumer: show failure
    end
    API-->BillingService: Start billing process
```
