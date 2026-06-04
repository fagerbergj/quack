# Agent Researcher

A research agent with chat interface, MCP server, REST API, and hexagonal architecture.

## Structure

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
├── frontend/openapi-ts.config.ts  # TypeScript client config
├── Makefile             # Build and test commands
└── .github/workflows/ci.yaml  # CI/CD pipeline
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
- OpenAPI Codegen CLI (optional, for code generation)

### Setup

```bash
# Install dependencies
cd frontend && npm install && cd ..

# Generate code from OpenAPI spec (optional)
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
go vet ./...

# Lint TypeScript
cd frontend && npx tsc --noEmit
```

### Build

```bash
# Build backend
go build -o agent-researcher ./server

# Build frontend
cd frontend && npm run build
```

## API Endpoints

### Health Check
- `GET /health` - Returns "ok" if service is healthy

### Research
- `POST /api/v1/research` - Perform research using LLM

### Chats
- `GET /api/v1/chats` - List all chats
- `POST /api/v1/chats` - Create a new chat
- `GET /api/v1/chats/{chat_id}` - Get chat with messages
- `DELETE /api/v1/chats/{chat_id}` - Delete a chat
- `POST /api/v1/chats/{chat_id}/messages` - Send a message

### MCP
- `GET /api/v1/research/mcp` - Get MCP server configuration

## MCP Tools

- `web_search` - Search the web for information
- `rag_search` - Semantic search across research knowledge base
- `summarize` - Generate a summary of content
