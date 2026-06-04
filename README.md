# Agent Researcher

A simple research agent with chat interface, MCP server, REST server, and ADK agent framework.

## Structure

```
agent-researcher/
├── frontend/         # React + Vite chat UI
├── server/           # Go REST server with MCP endpoint
│   ├── main.go       # Server entry point
│   ├── mcp.go        # MCP endpoint
│   └── research.go   # Research/LLM inference endpoint
└── ...
```

## Quick Start

### Frontend

```bash
cd frontend
npm install
npm run dev
```

### Backend

```bash
go mod download
go run server/main.go
```

