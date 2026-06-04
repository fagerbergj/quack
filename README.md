# Agent Researcher

A simple research agent with chat interface, MCP server, REST server, and ADK agent framework.

## Structure

- `frontend/` - React + Vite chat UI
- `server/` - Go REST server with MCP endpoint
- `adk/` - Google ADK agent integration

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
