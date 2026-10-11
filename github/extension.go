// Package github is quack's GitHub App extension: auth, tools, webhook dispatch.
package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const extensionName = "github"

// webhookPath is where the inbound webhook receiver is mounted, self-verifying via HMAC.
const webhookPath = "/webhook"

// runUserID distinguishes GitHub-driven sessions from local UI sessions
// when a comment carries no login (defensive; GitHub always sends one).
const runUserID = "github"

// reactionTimeout bounds the 👀 ack reaction on a mention.
const reactionTimeout = 10 * time.Second

func init() {
	sdk.Register(extensionName, factory)
}

// Labels names the label vocabulary quack:plan/implement/merge/etc map to in a deployment.
type Labels struct {
	Plan       string `yaml:"plan"`
	Implement  string `yaml:"implement"`
	Review     string `yaml:"review"`
	Merge      string `yaml:"merge"`
	PartialFix string `yaml:"partial_fix"`
	Fix        string `yaml:"fix"`
}

const (
	defaultMention         = "/quack"
	defaultAutoReviewLabel = "quack-auto-review"
	defaultPlanLabel       = "quack:plan"
	defaultImplementLabel  = "quack:implement"
	defaultMergeLabel      = "quack:merge"
	defaultPartialFixLabel = "quack:partial-fix"
	defaultFixLabel        = "quack:fix"
)

var validTriggers = map[string]bool{
	"mention": true, "pr_opened": true, "label": true,
	"issue_plan": true, "issue_implement": true, "merge": true,
	"ci_fix": true, "explain": true,
}

// config is this extension's YAML shape under extensions.github in quack.yaml.
type config struct {
	ClientID           string   `yaml:"client_id"`
	PrivateKey         string   `yaml:"private_key"`
	PrivateKeyPath     string   `yaml:"private_key_path"`
	WebhookSecret      string   `yaml:"webhook_secret"`
	Mention            string   `yaml:"mention"`
	Triggers           []string `yaml:"triggers"`
	AllowedUsers       []string `yaml:"allowed_users"`
	Labels             Labels   `yaml:"labels"`
	RunTimeoutMinutes  int      `yaml:"run_timeout_minutes"`
	AutoArchiveOnMerge bool     `yaml:"auto_archive_on_merge"`
	// APIBase overrides api.github.com - QA-only, points at tools/qa/github-mock.
	APIBase string `yaml:"api_base"`
}

// applyDefaults validates and fills in defaults.
func (c *config) applyDefaults(log func(string, ...any)) error {
	if err := c.validateCredentials(); err != nil {
		return err
	}
	return c.applyLabelDefaults(log)
}

// validateCredentials: client_id, exactly one of private_key/private_key_path, and a webhook_secret.
func (c *config) validateCredentials() error {
	if c.ClientID == "" {
		return fmt.Errorf("github: client_id is required")
	}
	if c.PrivateKey == "" && c.PrivateKeyPath == "" {
		return fmt.Errorf("github: needs one of private_key or private_key_path")
	}
	if c.PrivateKey != "" && c.PrivateKeyPath != "" {
		return fmt.Errorf("github: sets both private_key and private_key_path; use one")
	}
	if c.WebhookSecret == "" {
		return fmt.Errorf("github: webhook_secret is required")
	}
	return nil
}

// applyLabelDefaults: the non-credential defaults, trigger validation, and the allowed_users deny log.
func (c *config) applyLabelDefaults(log func(string, ...any)) error {
	if c.RunTimeoutMinutes <= 0 {
		c.RunTimeoutMinutes = 120
	}
	if c.Mention == "" {
		c.Mention = defaultMention
	}
	if len(c.Triggers) == 0 {
		c.Triggers = []string{"mention"}
	}
	for _, t := range c.Triggers {
		if !validTriggers[t] {
			return fmt.Errorf("github: triggers has unknown entry %q (want mention, pr_opened, label, issue_plan, issue_implement, merge, ci_fix, or explain)", t)
		}
	}
	if c.Labels.Review == "" {
		c.Labels.Review = defaultAutoReviewLabel
	}
	if c.Labels.Plan == "" {
		c.Labels.Plan = defaultPlanLabel
	}
	if c.Labels.Implement == "" {
		c.Labels.Implement = defaultImplementLabel
	}
	if c.Labels.Merge == "" {
		c.Labels.Merge = defaultMergeLabel
	}
	if c.Labels.PartialFix == "" {
		c.Labels.PartialFix = defaultPartialFixLabel
	}
	if c.Labels.Fix == "" {
		c.Labels.Fix = defaultFixLabel
	}
	if len(c.AllowedUsers) == 0 {
		log("github: allowed_users is empty; DENYING every human-invoked trigger " +
			"(mention comments, quack:plan/implement/merge labels) until it is set - auto-review is unaffected")
	}
	return nil
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	var cfg config
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("github: parse config: %w", err)
		}
	}
	logf := func(msg string, args ...any) {
		if host.Log != nil {
			host.Log.Warn(msg)
		}
	}
	if err := cfg.applyDefaults(logf); err != nil {
		return nil, err
	}
	if host.DataDir == "" {
		return nil, fmt.Errorf("github: Host.DataDir is required")
	}

	pem, err := LoadPrivateKey(cfg.PrivateKey, cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	app, err := NewApp(cfg.ClientID, pem)
	if err != nil {
		return nil, fmt.Errorf("github: init: %w", err)
	}
	app.SetPartialFixLabel(cfg.Labels.PartialFix)
	app.SetAPIBase(cfg.APIBase)
	app.SetFooter(host.Version, host.PublicURL)
	app.decide = host.Decide

	triggers := make(map[string]bool, len(cfg.Triggers))
	for _, t := range cfg.Triggers {
		triggers[t] = true
	}
	app.SetReviewCommands(cfg.Mention, cfg.Labels, triggers)
	allowedUsers := make(map[string]bool, len(cfg.AllowedUsers))
	for _, u := range cfg.AllowedUsers {
		allowedUsers[strings.ToLower(u)] = true
	}

	e := &Extension{
		app:                app,
		host:               host,
		store:              newStore(host.DataDir),
		secret:             []byte(cfg.WebhookSecret),
		mention:            cfg.Mention,
		triggers:           triggers,
		labels:             cfg.Labels,
		allowedUsers:       allowedUsers,
		runTimeout:         time.Duration(cfg.RunTimeoutMinutes) * time.Minute,
		autoArchiveOnMerge: cfg.AutoArchiveOnMerge,
	}
	if host.Classify != nil {
		e.intentClassifier = hostClassifier{host: host}
	}
	return e, nil
}

// hostClassifier adapts Host.Classify to IntentClassifier, which tests fake.
type hostClassifier struct{ host sdk.Host }

func (h hostClassifier) Classify(ctx context.Context, prompt string) (string, error) {
	return h.host.Classify(ctx, prompt)
}

// Extension is the GitHub App extension: tools + git auth + inbound webhook.
type Extension struct {
	app                *App
	host               sdk.Host
	store              *ghStore
	secret             []byte
	mention            string
	triggers           map[string]bool
	labels             Labels
	allowedUsers       map[string]bool // lower-cased; empty = deny all human-invoked triggers
	inflight           sync.Map        // sessionID → time.Time claim; leased dedup for concurrent triggers
	mergeMu            keyedMutex      // serializes merge-intent read-verdict-act per session
	pending            sync.Map        // globalChatID → *pendingRun; correlates RunEnded back to its dispatch
	runTimeout         time.Duration
	autoArchiveOnMerge bool
	bg                 sync.WaitGroup // every webhook-spawned goroutine (see spawn); Wait drains them

	// intentClassifier is Host.Classify when the host has one; nil degrades to intent.go's fallbacks.
	intentClassifier IntentClassifier
}

var (
	_ sdk.Extension           = (*Extension)(nil)
	_ sdk.Starter             = (*Extension)(nil)
	_ sdk.RunObserver         = (*Extension)(nil)
	_ sdk.Deliverer           = (*Extension)(nil)
	_ sdk.GitCredentialSource = (*Extension)(nil)
)

// isInvokerAllowed checks the configured allowlist. Empty list = deny all human-invoked triggers.
func (e *Extension) isInvokerAllowed(login string) bool {
	return e.allowedUsers[strings.ToLower(login)]
}

func (e *Extension) Tools() []tool.Tool { return e.app.Tools() }

// spawn runs f in a goroutine tracked by Wait, so a webhook handler can fire
// background work without outliving the store/HTTP client it uses.
func (e *Extension) spawn(f func()) {
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		f()
	}()
}

// Wait blocks until every spawned goroutine returns - call before closing
// anything spawned work still uses (store, HTTP client), or a completion races that close.
func (e *Extension) Wait() {
	e.bg.Wait()
}

// Start opens and migrates the store so a bad database fails boot; queries that
// race ahead of it (RunEnded from a resumed node) open it themselves.
func (e *Extension) Start(ctx context.Context) error {
	if _, err := e.store.conn(); err != nil {
		return err
	}
	// A row this old outlived its run's deadline and lease, so no RunEnded will ever consume it.
	if err := e.store.PrunePendingRuns(ctx, time.Now().Add(-(e.inflightLease() + e.runTimeout))); err != nil {
		e.host.Log.Warn("github: pending-run prune failed; stale rows stay until the next boot", "err", err)
	}
	// A run resumed at boot still holds its session: without its claim a new trigger dispatches a duplicate.
	claims, err := e.store.PendingClaims(ctx)
	if err != nil {
		e.host.Log.Warn("github: pending-run claims not restored; a trigger during a resumed run may duplicate it", "err", err)
	}
	for sessionID, at := range claims {
		e.inflight.LoadOrStore(sessionID, at)
	}
	return nil
}

// Deliver/GitCredential satisfy sdk.Deliverer/sdk.GitCredentialSource by delegating to App.
func (e *Extension) Deliver(ctx context.Context, dc sdk.DeliveryContext) ([]sdk.DeliveryItemOutcome, error) {
	return e.app.Deliver(ctx, dc)
}

func (e *Extension) GitCredential(ctx context.Context, rawURL string) (*sdk.GitCredential, error) {
	return e.app.GitCredential(ctx, rawURL)
}

// RegisterRoutes mounts the inbound webhook receiver on public; it verifies its own HMAC signature.
func (e *Extension) RegisterRoutes(authed chi.Router, public chi.Router) {
	public.Post(webhookPath, e.handleWebhook)
}

// globalChatID mirrors quack's "ext:<extension>:<localID>" namespacing (sdk.ChatRef.LocalID's contract),
// so RunEnded's namespaced id correlates back to the dispatched sessionID.
func globalChatID(sessionID string) string {
	return "ext:" + extensionName + ":" + sessionID
}
