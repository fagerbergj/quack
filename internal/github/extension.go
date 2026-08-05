package github

import (
	"context"
	"iter"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-chi/chi/v5"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// WebhookPath is where the inbound webhook receiver is mounted.
const WebhookPath = "/api/v1/github/webhook"

// runUserID distinguishes GitHub-driven sessions from local UI sessions.
const runUserID = "github"

// defaultRunTimeout when extensions.github.run_timeout_minutes is unset.
const defaultRunTimeout = 2 * time.Hour

// reactionTimeout bounds the 👀 ack reaction on a mention.
const reactionTimeout = 10 * time.Second

// Runner is the orchestrator subset the webhook needs — keeps the extension decoupled.
type Runner interface {
	Run(ctx context.Context, userID, sessionID, message string, attachments []*genai.Part) iter.Seq2[stream.SSEEvent, error]
	// LatestAnswer returns the final assistant text persisted for a session
	// after a run drains.
	LatestAnswer(ctx context.Context, userID, sessionID string) string
	// ResetSession deletes history for a label-driven work request (T4 session hygiene).
	ResetSession(ctx context.Context, userID, sessionID string) error
}

// Extension is the GitHub App extension: tools + git auth + inbound webhook.
type Extension struct {
	app             *App
	secret          []byte
	mention         string
	triggers        map[string]bool
	labels          config.GitHubLabels
	allowedUsers    map[string]bool // lower-cased; empty = deny all human-invoked triggers
	runner          Runner
	store           *store.Store
	hub             *stream.Hub
	eventLog        *runlog.EventLog
	inflight        sync.Map // sessionID → struct{}{}; dedup for concurrent triggers (#665, #668)
	runTimeout      time.Duration
	intentClassifier IntentClassifier // nil degrades to conversational
	jail             *workspace.Jail  // nil skips context-dir writing
	workspaceUserID  string
}

// SetIntentClassifier wires the mention intent classifier for the envelope builder. Optional.
func (e *Extension) SetIntentClassifier(c IntentClassifier) {
	e.intentClassifier = c
}

// SetJail wires the workspace jail for the sibling context directory (#660).
func (e *Extension) SetJail(j *workspace.Jail, workspaceUserID string) {
	e.jail = j
	e.workspaceUserID = workspaceUserID
}

// NewExtension wraps an App with webhook config and a Runner.
func NewExtension(app *App, cfg config.GitHubExtensionConfig, runner Runner, st *store.Store, hub *stream.Hub) *Extension {
	cfgTriggers := cfg.Triggers
	if len(cfgTriggers) == 0 {
		cfgTriggers = []string{"mention"}
	}
	triggers := make(map[string]bool, len(cfgTriggers))
	for _, t := range cfgTriggers {
		triggers[t] = true
	}
	labels := cfg.Labels
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
	if labels.PartialFix == "" {
		labels.PartialFix = "quack:partial-fix"
	}
	if labels.Fix == "" {
		labels.Fix = "quack:fix"
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
	allowedUsers := make(map[string]bool, len(cfg.AllowedUsers))
	for _, u := range cfg.AllowedUsers {
		allowedUsers[strings.ToLower(u)] = true
	}
	return &Extension{
		app:          app,
		secret:       []byte(cfg.WebhookSecret),
		mention:      cfg.Mention,
		triggers:     triggers,
		labels:       labels,
		allowedUsers: allowedUsers,
		runner:       runner,
		store:        st,
		hub:          hub,
		eventLog:     eventLog,
		runTimeout:   runTimeout,
	}
}

// isInvokerAllowed checks the configured allowlist. Empty list = deny all human-invoked triggers.
func (e *Extension) isInvokerAllowed(login string) bool {
	return e.allowedUsers[strings.ToLower(login)]
}

// App exposes the underlying auth as a git-credential source.
func (e *Extension) App() *App { return e.app }

func (e *Extension) Name() string       { return "github" }
func (e *Extension) Tools() []tool.Tool { return e.app.Tools() }

// RegisterRoutes mounts the inbound webhook receiver.
func (e *Extension) RegisterRoutes(r chi.Router) {
	r.Post(WebhookPath, e.handleWebhook)
}
