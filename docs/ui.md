# Web UI

The web SPA (`frontend/`, React 19 + Vite + Tailwind) is a graphical front end over the same REST + SSE API the CLI uses - a chat list, a composer, and a live view of a run's DAG (per-node status, streamed tokens/thinking/tool calls, pause/resume/retry) as it executes.

## Running it

- **Production / everyday use** - `quack server run` serves it already; the frontend build is compiled and embedded into the Go binary (`make build` / `make frontend-build`), so it's just whatever address the server listens on, no separate process.
- **Frontend development** - `cd frontend && npm run dev` for a hot-reload dev server on `:5173`, talking to a `quack server run` instance for its API.

## What it can do

Everything the [CLI](cli.md) can, graphically:

- Create a chat and send messages (`ChatList`, `Composer`).
- Watch a run stream in - tokens, thinking, tool calls/results - grouped by node and by stage (worker / judge / revise), via `DagView`/`DagNode`.
- Attach files (image/audio) to a message.
- Answer a paused question inline, and drive per-node control (pause, resume, retry, edit, queue) the same way `quack chat node ...` does.

## Auth

The SPA authenticates the same way any REST client does - see [`docs/api.md`](api.md#auth) and [`docs/configuration/auth.md`](configuration/auth.md).
Behind a trusted gateway (forward-auth), it never runs its own login flow; identity arrives via the configured `trusted_headers`.

## Contributing to it

State lives in `frontend/src/state/` (`chatStore.ts`, `agentStream.ts` parsing the SSE stream, `ChatStoreProvider.tsx`).
Components under `frontend/src/components/` are co-located with Storybook stories and vitest tests.
The `frontend-design` skill covers the re-render-isolation and streaming-chat patterns this UI relies on - load it before restyling or extending anything under `frontend/src/`.
