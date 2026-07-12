package github

import (
	"context"
	"iter"
	"time"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-chi/chi/v5"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/stream"
)

// WebhookPath is where the inbound webhook receiver is mounted.
const WebhookPath = "/api/v1/github/webhook"

// runUserID is the identity webhook-driven runs persist under — distinct from
// the local UI's "local" user, so App runs keep their own sessions.
const runUserID = "github"

// runTimeout bounds a single webhook-driven orchestrator run.
const runTimeout = 2 * time.Hour

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
	app     *App
	secret  []byte
	mention string
	runner  Runner
}

// NewExtension wraps an already-built App (serve constructs the App early so it
// can also serve as the git-credential source before the orchestrator exists)
// with the webhook config and a Runner.
func NewExtension(app *App, cfg config.GitHubExtensionConfig, runner Runner) *Extension {
	return &Extension{
		app:     app,
		secret:  []byte(cfg.WebhookSecret),
		mention: cfg.Mention,
		runner:  runner,
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
