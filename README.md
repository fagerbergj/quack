# Agent Researcher

## Build and Run

```bash
go build -o agent-researcher ./server
./agent-researcher
```

## Test

```bash
go test ./...
go vet ./...
gofmt -w .
```
agent-researcher/
├── frontend/            # React + Vite frontend with chat UI
│   ├── src/
│   │   ├── components/  # Reusable components
│   │   ├── pages/       # Page components
│   │   ├── state/       # State management
│   │   ├── api.ts       # API wrapper
│   │   └── main.tsx     # Entry point
│   └── package.json
├── server/              # Go backend with hexagonal architecture
│   ├── main.go          # Entry point
│   ├── core/
│   │   ├── service.go       # Service interfaces
│   │   ├── service_impl.go  # Service implementations
│   │   ├── port/            # Port interfaces
│   │   └── service_test.go  # Service tests
│   ├── api/
│   │   ├── rest/        # REST API handlers
│   │   ├── mcp/         # MCP API handlers
│   │   └── schema/      # Generated types
│   └── adapters/        # External service adapters
├── openapi.yaml         # OpenAPI specification
├── mcp.json             # MCP configuration
├── scripts/
│   └── generate.sh      # Code generation script
├── .oapi-codegen.yaml   # OpenAPI Codegen config
└── frontend/openapi-ts.config.ts  # TypeScript client config
```

## Hexagonal Architecture

The server follows hexagonal architecture (ports and adapters):

- **Core Layer**: Business logic and interfaces (ports)
- **API Layer**: REST and MCP handlers that translate HTTP to core
- **Adapters Layer**: External services (database, LLM, etc.)

## Development

### Prerequisites

- Node.js 18+ and npm
- Go 1.23+
- OpenAPI Codegen CLI

### Setup

```bash
# Install dependencies
cd frontend && npm install && cd ..

# Generate code from OpenAPI spec
./scripts/generate.sh
```

### Running

```bash
# Backend
go run server/main.go

# Frontend (in another terminal)
cd frontend && npm run dev
```

### Testing

```bash
# Run Go tests
go test ./...

# Run frontend tests
cd frontend && npm test
```

### Linting

```bash
# Lint Go
gofmt -w .

# Lint TypeScript
cd frontend && npx tsc --noEmit
```
