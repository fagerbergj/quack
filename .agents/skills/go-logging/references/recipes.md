# Go Logging Recipes (log/slog)

Concrete code for the decisions in `SKILL.md`.
Copy and adapt; don't re-derive.

## 1. Handler setup (quack: `cmd/server/main.go`)

Called as the first statement in `main()`, before anything else logs (GORM grabs `slog.Default()` at `store.Open`):

```go
func setupLogging() {
	// slog.Level implements TextUnmarshaler: "" and unknown values error out,
	// leaving the zero value LevelInfo - our intended default.
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(os.Getenv("QUACK_LOG_LEVEL")))
	opts := &slog.HandlerOptions{Level: lvl}
	// stdout: logs are the server's output; let the orchestration layer ship them.
	var h slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if strings.EqualFold(os.Getenv("QUACK_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}
```

`SetDefault` also reroutes any stray stdlib `log.*` (and ADK's) through this handler.

**Runtime re-leveling (not currently used):** swap the fixed level for a `*slog.LevelVar` and expose `level.Set(slog.LevelDebug)` via a signal/admin endpoint.
Add only when restart-to-change is actually too slow - quack uses the env var.

## 2. Fatal - main only

slog has no Fatal (it would skip deferred cleanup below `main`).
One helper:

```go
// fatal logs at Error and exits - slog has no Fatal of its own.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
```

Below `main`, **return** the error instead (`return fmt.Errorf("config: %w", err)`).

## 3. Per-instance logger field (objects with stable identity)

For a struct built per unit of work whose logs all share identity attrs (the vetting gate, built per worker).
Set it in the **constructor** (runtime → after `SetDefault`, so it binds the real handler):

```go
type gate struct {
	name string
	log  *slog.Logger // pre-tagged with component=vetting + agent name
	// …
}

func NewGatedAgent(worker adkagent.Agent, /* … */) (*GatedAgent, error) {
	g := &gate{name: worker.Name() /* … */}
	g.log = slog.With("component", "vetting", "agent", g.name)
	// …
}

// usage - identity is implicit, only the event-specific attrs at the call site:
g.log.Debug("judge round done", "round", round, "dur", time.Since(tj), "score", v.Score, "passed", passed)
```

Do **not** do this as a package-level `var` - see the init-order gotcha in `SKILL.md`.
Package-level or one-off sites pass `component` inline: `slog.Info("node done", "component", "dag", "node", id, …)`.

## 4. Bridge a third-party logger (GORM) through slog

`slog.NewLogLogger` adapts the slog handler into the `*log.Logger` that `gorm/logger` wants, so GORM's slow-query warnings share the app handler:

```go
gormCfg := &gorm.Config{Logger: logger.New(
	slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	logger.Config{
		SlowThreshold:             200 * time.Millisecond, // log slow queries only, not every query
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
	},
)}
```

## 5. Secret / PII redaction

Quack is self-hosted with no compliance mandate today, so redaction is light - but never log passwords, API keys, tokens, session cookies, connection strings, or raw user PII.
Mechanisms, in order of preference:

**a. `LogValuer` on a distinct type** - best for a known sensitive value:

```go
type APIToken string
func (t APIToken) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }
```

⚠️ **Bypassed on struct fields reached via reflection.** Logging a whole struct that *contains* an `APIToken` field still leaks it.
Either don't log whole structs, or use (b)/(c).

**b. `ReplaceAttr` deny-list at the handler** - catches by key regardless of nesting:

```go
opts := &slog.HandlerOptions{
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		switch a.Key {
		case "password", "api_key", "token", "authorization":
			a.Value = slog.StringValue("[REDACTED]")
		}
		return a
	},
}
```

**c. `github.com/m-mizutani/masq`** - hooks `ReplaceAttr` for automatic *deep* redaction of nested struct fields by type, struct tag, or field name.
Reach for it only if structs with secrets are genuinely being logged; a new dependency is otherwise YAGNI for this repo.

## 6. Request correlation (child logger + context) - pattern for HTTP-scoped work

Not yet wired in quack, but the shape when adding it:

```go
// middleware: one request-scoped logger, stored in context
reqLog := slog.With("request_id", reqID, "user_id", userID)
ctx := context.WithValue(r.Context(), logKey{}, reqLog)

// downstream: pull it (or fall back to default) and every line carries request_id
func logOf(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(logKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
```

Always pass `context.Context` into spawned goroutines so they inherit the same `request_id`/trace.
When an OTel handler lands (M12), switch hot-path calls to the `*Context` variants (`logger.InfoContext(ctx, …)`) so the handler can pull span IDs - until then plain calls are correct, since no handler reads the context.

## 7. Canonical log line (one dense summary per request)

Instead of scattering fragments, emit a single completion line carrying the whole request outcome:

```go
reqLog.Info("request completed",
	"http_method", r.Method, "http_path", r.URL.Path, "http_status", status,
	"duration_ms", time.Since(start).Milliseconds(), "db_queries", nQueries)
```

Quack's node/gate completion summaries ("node done", "vetted answer ready") are the same idea at the DAG-node grain.
