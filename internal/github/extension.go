package github

import (
	"context"
	"iter"
	"sync"
	"time"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-chi/chi/v5"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// WebhookPath is where the inbound webhook receiver is mounted.
const WebhookPath = "/api/v1/github/webhook"

// runUserID is the identity webhook-driven runs persist under — distinct from
// the local UI's "local" user, so App runs keep their own sessions.
const runUserID = "github"

// defaultRunTimeout bounds a single webhook-driven orchestrator run when
// extensions.github.run_timeout_minutes is unset (mirrors config.applyDefaults
// for callers that build the struct directly).
const defaultRunTimeout = 2 * time.Hour

// reactionTimeout bounds the deterministic 👀 acknowledgment reaction on a
// mention — a quick, best-effort POST that must not linger.
const reactionTimeout = 10 * time.Second

// Runner is the subset of the orchestrator the webhook needs: dispatch a run
// and read its final answer. Defined here (not imported) so the extension stays
// decoupled from the orchestrator package.
type Runner interface {
	Run(ctx context.Context, userID, sessionID, message string, attachments []*genai.Part) iter.Seq2[stream.SSEEvent, error]
	// LatestAnswer returns the final assistant text persisted for a session
	// after a run drains.
	LatestAnswer(ctx context.Context, userID, sessionID string) string
}

// Extension is the GitHub App extension: outbound tools + git auth + the inbound
// webhook, all sharing the App's installation-token auth. Implements
// extension.Extension.
type Extension struct {
	app      *App
	secret   []byte
	mention  string
	triggers map[string]bool // configured trigger set: mention, pr_opened, label, issue_plan
	labels   config.GitHubLabels
	runner   Runner
	store    *store.Store     // nil in tests that don't need URL persistence
	hub      *stream.Hub      // shared with the REST handler, nil when store is nil
	eventLog *runlog.EventLog // nil when store is nil (no durable persistence to do)
	// runLocks serialises dispatches per PR session: a follow-up (or a rapid
	// re-label) that arrives while a run on the same PR is in flight WAITS instead
	// of running concurrently on the same session — concurrent runs on one session
	// corrupt each other (garbled answers, cross-run tool events). sessionID →
	// *sync.Mutex.
	runLocks sync.Map
	// runTimeout bounds one webhook-driven run (extensions.github.run_timeout_minutes).
	runTimeout time.Duration
}

// sessionLock returns the per-session mutex, creating it on first use.
func (e *Extension) sessionLock(sessionID string) *sync.Mutex {
	m, _ := e.runLocks.LoadOrStore(sessionID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// NewExtension wraps an already-built App (serve constructs the App early so it
// can also serve as the git-credential source before the orchestrator exists)
// with the webhook config and a Runner. hub is the *stream.Hub shared with the
// REST handler (nil gets a private one) — so a webhook-dispatched run's events
// reach a browser watching that chat, same as any UI-initiated run's would.
func NewExtension(app *App, cfg config.GitHubExtensionConfig, runner Runner, st *store.Store, hub *stream.Hub) *Extension {
	cfgTriggers := cfg.Triggers
	if len(cfgTriggers) == 0 {
		cfgTriggers = []string{"mention"} // config.applyDefaults normally does this; re-default here so callers that build the struct directly (tests) still get mention-only behavior
	}
	triggers := make(map[string]bool, len(cfgTriggers))
	for _, t := range cfgTriggers {
		triggers[t] = true
	}
	labels := cfg.Labels
	// config.applyDefaults normally fills these; re-default here so callers that
	// build the struct directly (tests) still get the standard label names.
	if labels.Review == "" {
		labels.Review = cfg.AutoReviewLabel
	}
	if labels.Review == "" {
		labels.Review = "quack-auto-review"
	}
	if labels.Plan == "" {
		labels.Plan = "quack:plan"
	}
	if labels.Implement == "" {
		labels.Implement = "quack:implement"
	}
	if labels.Merge == "" {
		labels.Merge = "quack:merge"
	}
	if hub == nil {
		hub = stream.NewHub()
	}
	var eventLog *runlog.EventLog
	if st != nil {
		eventLog = runlog.NewEventLog(st)
	}
	runTimeout := defaultRunTimeout
	if cfg.RunTimeoutMinutes > 0 {
		runTimeout = time.Duration(cfg.RunTimeoutMinutes) * time.Minute
	}
	return &Extension{
		app:        app,
		secret:     []byte(cfg.WebhookSecret),
		mention:    cfg.Mention,
		triggers:   triggers,
		labels:     labels,
		runner:     runner,
		store:      st,
		hub:        hub,
		eventLog:   eventLog,
		runTimeout: runTimeout,
	}
}

// App exposes the underlying auth so the caller can wire it as the git-credential
// source (tools.GitTokenSource) — the App itself implements GitCredential.
func (e *Extension) App() *App { return e.app }

func (e *Extension) Name() string       { return "github" }
func (e *Extension) Tools() []tool.Tool { return e.app.Tools() }

// RegisterRoutes mounts the inbound webhook receiver.
func (e *Extension) RegisterRoutes(r chi.Router) {
	r.Post(WebhookPath, e.handleWebhook)
}
