---
name: go-testing
description: |
  Best practices for unit and integration testing Go servers and TUIs — stdlib-first. Covers table-driven
  tests with t.Run subtests + t.Parallel, httptest (NewRequest/NewRecorder for handlers, NewServer for
  full-chain integration + third-party API stubs), testing middleware in isolation then composed,
  interface-based dependency injection with function-field mocks (and when gomock/mockery/testify earn
  their keep), real-database testing via Testcontainers over sqlmock, benchmarking with the Go 1.24+
  testing.B.Loop pattern, testing Charm Bubble Tea TUIs (WithInput/WithOutput/WithoutRenderer, teatest
  golden files), and a CI matrix (go test -race -coverprofile, unit-vs-integration gating).
  Use when writing or reviewing any *_test.go in the quack Go backend (internal/, cmd/) or the Bubble Tea
  TUI (internal/tui/) — picking a test shape, mocking a dependency, testing an HTTP handler/middleware,
  or wiring CI. Do NOT use for frontend (vitest/MSW — see frontend-design) or non-Go code.
license: MIT
metadata:
  author: jason
  version: "1.0"
---

# Go Testing — Servers & TUIs

## Overview

Test Go with the standard library first. `go test` is the only runner; `testing`, `net/http/httptest`,
and interface injection cover ~95% of cases. Reach for a third-party package only when it earns its place
(`require` vs `assert`, golden files, gomock call-order assertions). This skill is the decision layer —
which test shape, what to mock, real DB vs fake. Concrete patterns are inline below.

## When to Use

- Writing or reviewing a `*_test.go` under `internal/` or `cmd/` (HTTP handlers, middleware, services, DAG nodes).
- Testing the Charm Bubble Tea TUI under `internal/tui/`.
- Deciding how to mock a dependency, or whether to use a real database.
- Adding/auditing the test job in CI.

## When NOT to Use

- Frontend tests — vitest + MSW, see `frontend-design`.
- Non-Go code.

## The decisions

### 1. Default shape: table-driven + subtests

One `TestXxx(*testing.T)` per behavior; a `[]struct` of cases iterated with `t.Run(tt.name, ...)`.
Each case is **named** so failures point at the exact row. Add `t.Parallel()` inside the subtest only
when cases share no state. Prefer an external test package (`package foo_test`) to test the public API.

```go
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        handler := &UserHandler{Service: tt.mockService}
        req := httptest.NewRequest(http.MethodGet, "/users?id="+tt.userID, nil)
        rec := httptest.NewRecorder()
        handler.GetUser(rec, req)
        if rec.Code != tt.wantCode {
            t.Errorf("status = %d; want %d", rec.Code, tt.wantCode)
        }
    })
}
```

### 2. HTTP: httptest, two levels

- **Unit (handler in isolation):** `httptest.NewRequest` + `httptest.NewRecorder`, call `ServeHTTP`,
  assert `rec.Code` / `rec.Header()` / `rec.Body.String()`. No server starts.
- **Integration (real routing + middleware + auth):** `httptest.NewServer(router)` (`defer srv.Close()`),
  hit it with a real `http.Client`/`http.Get(srv.URL + ...)`. Exercises the whole chain.

### 3. Middleware: isolate, then compose

Test the middleware alone against a stub `next` handler (table of header/status cases), **then** once as
part of the full chain to catch ordering bugs. Both, not one.

### 4. Mocking: interfaces + function fields first

Accept interfaces, not concrete types. The stdlib-only mock is a struct of function fields:

```go
type MockUserService struct {
    GetUserByIDFunc func(id string) (*User, error)
}
func (m *MockUserService) GetUserByID(id string) (*User, error) {
    if m.GetUserByIDFunc != nil { return m.GetUserByIDFunc(id) }
    return nil, errors.New("not implemented")
}
```

Zero deps, sufficient for most cases. Escalate only when justified:
- **`gomock`/`mockgen`** — when you must assert call *order*/counts on generated, type-safe mocks.
- **`mockery`** — simpler CLI generation from interfaces.
- **`testify`** — `require` (halt on failed precondition) vs `assert` (collect failures), or golden-file
  comparisons. Lean stdlib otherwise.
- **Third-party API** — stub it with `httptest.NewServer` and point your client's `BaseURL` at `stub.URL`.

### 5. Database: real over sqlmock

Prefer **Testcontainers for Go** (real `postgres:16-alpine` in Docker) over `sqlmock` — sqlmock hides
constraint violations, JSON operators, and transaction isolation. `defer container.Terminate(ctx)`,
pull the conn string, point the repo at it. Gate these behind the integration split (section 8).

### 6. Benchmarks: Go 1.24+ `b.Loop()`

```go
func BenchmarkHandler(b *testing.B) {
    handler := http.HandlerFunc(healthHandler)
    req := httptest.NewRequest(http.MethodGet, "/health", nil)
    b.ReportAllocs()
    for b.Loop() {                       // preferred over `for i := 0; i < b.N; i++`
        rec := httptest.NewRecorder()
        handler.ServeHTTP(rec, req)
    }
}
```

`b.Loop()` prevents dead-code elimination and auto-excludes setup before the loop — no `ResetTimer`/
`StopTimer` needed. Run `go test -bench=. -benchmem`; `-count` to average, `-cpu` for parallelism.

### 7. TUIs: Bubble Tea

- **Stdlib-ish:** `tea.NewProgram(model{}, tea.WithInput(&in), tea.WithOutput(&buf), tea.WithoutRenderer())`
  — feed keystrokes via `in`, assert on `buf.String()`. `WithoutRenderer()` skips paint for speed.
- **Golden files + model state:** `teatest.NewTestModel(t, m, teatest.WithInitialTermSize(w,h))`,
  read `tm.FinalOutput(t)`, compare with `teatest.RequireEqualOutput(t, out)` (updates on `-update`).
- The fastest TUI tests drive `model.Update()` directly with `tea.Msg`s and assert on returned state —
  no program loop. (See `quack-cli` for quack's three-tier TUI testing strategy.)

### 8. CI

```yaml
- run: go test -race -coverprofile=coverage.out -covermode=atomic ./...
```

- `-race` for concurrent code; `-covermode=atomic` is the thread-safe coverage mode.
- Matrix over the latest two Go minors.
- **Split unit from integration:** fast unit tests (no Docker) first; gate Testcontainers tests behind a
  build tag or env (`GO_TEST_INTEGRATION=1`) so the common path stays quick.

## Quack notes

- Backend tests already follow table-driven + `httptest`; match that. CI runs `go test ./...` (see AGENTS.md) —
  keep new tests passing under `-race`.
- TUI lives at `internal/tui/` (Bubble Tea); the `quack-cli` skill owns quack's specific TUI test tiers.
