// Package serve is Quack's server bootstrap: config, inference, orchestrator, stores, REST + MCP + SPA.
package serve

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/acp"
	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server"
	mcpserver "github.com/fagerbergj/quack/internal/server/mcp"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// localUserID is the single-user identity every filesystem/git tool resolves against.
const localUserID = "local"

// dotagentsEmbeddedSkills: the go:embed'd dotagents snapshot (SOURCE.md names its pin). Startup requires
// format-markdown and plan-work, so a standalone install must find them without disk discovery.
const dotagentsEmbeddedSkills = ".agents/embedded/dotagents/skills"

// activeKey mirrors dag.AdmissionSpec.residencyKey (provider+role) for Limits.Active.
func activeKey(provider, role string) string { return provider + "\x00" + role }

// buildAdmission builds the capacity ledger; limits absent from the registries are omitted, which
// Admission treats as unlimited.
func buildAdmission(cfg *config.Config) *dag.Admission {
	sessions := map[string]int{}
	kv := map[string]int{}
	for name, mc := range cfg.Models {
		if mc.Limits == nil {
			continue
		}
		if mc.Limits.Sessions > 0 {
			sessions[name] = mc.Limits.Sessions
		}
		if mc.Limits.KVTokens > 0 {
			kv[name] = mc.Limits.KVTokens
		}
	}
	active := map[string]int{}
	for provider, pc := range cfg.Providers {
		if pc.Limits == nil {
			continue
		}
		for role, n := range pc.Limits.Active {
			active[activeKey(provider, role)] = n
		}
	}
	return dag.NewAdmission(sessions, kv, active, 0)
}

// admissionSpecFor: an agent's model provider/role for residency, and its effective context window
// (agent override, else model default) as the kv_tokens reservation.
func admissionSpecFor(cfg *config.Config) func(agentName string) dag.AdmissionSpec {
	return func(agentName string) dag.AdmissionSpec {
		ac, ok := cfg.Agents[agentName]
		if !ok || ac.Model == "" {
			return dag.AdmissionSpec{}
		}
		mc, ok := cfg.Models[ac.Model]
		if !ok {
			return dag.AdmissionSpec{}
		}
		return modelSpec(mc, ac.Model, ac.ContextWindow)
	}
}

// modelSpec builds one AdmissionSpec from a model's registry entry.
// ctxWindow is the caller's override; 0 falls back to the model's default.
func modelSpec(mc config.ModelConfig, name string, ctxWindow int) dag.AdmissionSpec {
	if ctxWindow == 0 {
		ctxWindow = mc.ContextWindow
	}
	spec := dag.AdmissionSpec{Model: name, KVTokens: ctxWindow, Provider: mc.Provider, Role: mc.Role}
	if mc.Limits == nil || mc.Limits.KVTokens == 0 {
		spec.KVTokens = 0 // absent kv_tokens = context isn't a scheduling dimension
	}
	return spec
}

// orchestratorSpec: when orchestrator.model reuses a worker model, both contend for that model's one
// sessions pool.
func orchestratorSpec(cfg *config.Config) dag.AdmissionSpec {
	mc, ok := cfg.Models[cfg.Orchestrator.Model]
	if !ok {
		return dag.AdmissionSpec{}
	}
	spec := modelSpec(mc, cfg.Orchestrator.Model, cfg.Orchestrator.ContextWindow)
	if cfg.Orchestrator.ContextWindow == 0 {
		// The orchestrator has no declared window; modelSpec would reserve the model's whole kv budget and
		// block every node it planned.
		spec.KVTokens = 0
	}
	return spec
}

// judgeSpec: the gate judge's own capacity spec, from gates.judge - shared by
// every node's judge round, since one judge model serves the whole plan.
func judgeSpec(cfg *config.Config) dag.AdmissionSpec {
	if !cfg.Gates.JudgeEnabled() {
		return dag.AdmissionSpec{}
	}
	mc, ok := cfg.Models[cfg.Gates.Judge.Model]
	if !ok {
		return dag.AdmissionSpec{}
	}
	return modelSpec(mc, cfg.Gates.Judge.Model, cfg.Gates.Judge.ContextWindow)
}

// lightweightSpec: capacity spec for a call outside any held node reservation
// (classify/title/hook, hooks, consolidation) - sessions and residency, no kv.
func lightweightSpec(cfg *config.Config, modelName string) dag.AdmissionSpec {
	mc, ok := cfg.Models[modelName]
	if !ok {
		return dag.AdmissionSpec{}
	}
	return dag.AdmissionSpec{Model: modelName, Provider: mc.Provider, Role: mc.Role}
}

// resolvedSkillSource wraps each registered plugin's skills/ in Tolerant and Prefixed on the registry
// row name (not plugin.json's). The embedded "quack" plugin is excluded.
func resolvedSkillSource(plugins []plugin.Plugin) skill.Source {
	var sources []skill.Source
	for _, p := range plugins {
		if p.SkillsDir == "" {
			continue
		}
		dirFS := os.DirFS(p.SkillsDir)
		src := skillsource.Tolerant(skillsource.NewFileSystemSource(dirFS), dirFS, p.SkillsDir)
		sources = append(sources, skillsource.Prefixed(p.Name, src))
	}
	return skill.NewMergedSource(sources...)
}

// quackOwnSkillSource and dotagentsEmbeddedSkillSource are the embedded "quack:" plugin's two halves,
// split because registered plugins shadow them by different rules (exact vs bare name).
func quackOwnSkillSource() skill.Source {
	bundleFS := bundledir.SubFS("skills")
	return skillsource.Prefixed("quack", skillsource.Tolerant(skillsource.NewFileSystemSource(bundleFS), bundleFS, "bundled skills"))
}

func dotagentsEmbeddedSkillSource() skill.Source {
	daFS := bundledir.SubFS(dotagentsEmbeddedSkills)
	return skillsource.Prefixed("quack", skillsource.Tolerant(skillsource.NewFileSystemSource(daFS), daFS, "dotagents embedded skills"))
}

// resolvedHaveSets: qualified and bare skill names already served by registered plugins.
func resolvedHaveSets(plugins []plugin.Plugin) (qualified, bare map[string]bool) {
	fms, _ := resolvedSkillSource(plugins).ListFrontmatters(context.Background())
	qualified = make(map[string]bool, len(fms))
	bare = make(map[string]bool, len(fms))
	for _, fm := range fms {
		qualified[fm.Name] = true
		bare[skillsource.BareName(fm.Name)] = true
	}
	return qualified, bare
}

// missingQuackOwnSkillNames: quack's own skills not shadowed by exact qualified name.
func missingQuackOwnSkillNames(haveQualified map[string]bool) []string {
	var missing []string
	if own, err := quackOwnSkillSource().ListFrontmatters(context.Background()); err == nil {
		for _, fm := range own {
			if !haveQualified[fm.Name] {
				missing = append(missing, fm.Name)
			}
		}
	}
	return missing
}

// missingDotagentsEmbeddedSkillNames: embedded dotagents skills no resolved plugin serves by bare name,
// so an on-disk dotagents under any registry name suppresses the embedded copy.
func missingDotagentsEmbeddedSkillNames(haveBare map[string]bool) []string {
	var missing []string
	if da, err := dotagentsEmbeddedSkillSource().ListFrontmatters(context.Background()); err == nil {
		for _, fm := range da {
			if !haveBare[skillsource.BareName(fm.Name)] {
				missing = append(missing, fm.Name)
			}
		}
	}
	return missing
}

// missingQuackSkillNames is the full embedded backfill list; acpSkillPaths checks the halves separately
// because a raw on-disk skills/ dir can satisfy the own half.
func missingQuackSkillNames(plugins []plugin.Plugin) []string {
	haveQualified, haveBare := resolvedHaveSets(plugins)
	return append(missingQuackOwnSkillNames(haveQualified), missingDotagentsEmbeddedSkillNames(haveBare)...)
}

// newSkillSource: registered plugins' skills, backfilled with any embedded "quack:" skill none shadows.
func newSkillSource(plugins []plugin.Plugin) skill.Source {
	resolved := resolvedSkillSource(plugins)
	backfill := missingQuackSkillNames(plugins)
	if len(backfill) == 0 {
		return resolved
	}
	embedded := skill.NewMergedSource(quackOwnSkillSource(), dotagentsEmbeddedSkillSource())
	return skill.NewMergedSource(resolved, skillsource.Scoped(embedded, backfill))
}

// swappableSkillSource swaps its backing source atomically, so a REST plugin change reaches a built
// SkillToolset's next round without a restart.
type swappableSkillSource struct {
	cur atomic.Pointer[skill.Source]
}

func newSwappableSkillSource(initial skill.Source) *swappableSkillSource {
	s := &swappableSkillSource{}
	s.cur.Store(&initial)
	return s
}

func (s *swappableSkillSource) Swap(src skill.Source) { s.cur.Store(&src) }

func (s *swappableSkillSource) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	return (*s.cur.Load()).ListFrontmatters(ctx)
}

func (s *swappableSkillSource) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	return (*s.cur.Load()).LoadFrontmatter(ctx, name)
}

func (s *swappableSkillSource) LoadInstructions(ctx context.Context, name string) (string, error) {
	return (*s.cur.Load()).LoadInstructions(ctx, name)
}

func (s *swappableSkillSource) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	return (*s.cur.Load()).LoadResource(ctx, name, resourcePath)
}

func (s *swappableSkillSource) ListResources(ctx context.Context, name, subpath string) ([]string, error) {
	return (*s.cur.Load()).ListResources(ctx, name, subpath)
}

// LedgerStoreFromConfig resolves the ledger (WAL) backend from stores; config
// validation already guarantees a named store is Postgres.
func LedgerStoreFromConfig(cfg *config.Config) ledger.LedgerStore {
	name := cfg.Observability.Recording.Store
	if name == "" {
		return nil
	}
	s, ok := cfg.Store(name)
	if !ok {
		return nil
	}
	store, err := ledger.NewPGStoreFromURL(s.URL)
	if err != nil {
		slog.Warn("ledger store init failed; no WAL and no recording for this run", "component", "startup", "err", err)
		return nil
	}
	return store
}

// setDefaultAgent stamps m's metrics-only agent fallback for consumers that run outside a node's
// coords-stamped ctx. No-op when m doesn't implement it.
func setDefaultAgent(m any, name string) {
	if da, ok := m.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent(name)
	}
}

// BuildArtifactService resolves cfg.Artifacts: in-memory by default, or the
// durable Postgres large-object backend for a named store.
func BuildArtifactService(cfg *config.Config) (artifact.Service, error) {
	if cfg.Artifacts.Store == "" {
		return artifact.InMemoryService(), nil
	}
	as, ok := cfg.Store(cfg.Artifacts.Store)
	if !ok {
		return nil, fmt.Errorf("artifacts store %q not found in stores registry", cfg.Artifacts.Store)
	}
	return store.NewArtifactService(as.URL)
}

// warnIfEpisodicRecordsWontSurvive: artifacts.store defaults to in-memory, so `artifact:` records die on
// restart with no other signal. Loud, not fatal.
func warnIfEpisodicRecordsWontSurvive(cfg *config.Config) {
	if cfg.Artifacts.Store != "" {
		return
	}
	for _, w := range cfg.Workflows {
		for _, n := range w.Nodes {
			if n.Artifact != "" {
				slog.Warn("workflow node declares an episodic artifact but artifacts.store is unset (in-memory); records will not survive a restart",
					"component", "serve", "workflow", w.Name, "node", n.ID, "artifact", n.Artifact)
				return
			}
		}
	}
}

//go:embed all:web/dist
var webDist embed.FS

// Version is stamped by cmd/quack at build time.
var Version string

// Run builds the server and serves on cfg.Server.Addr until ctx is cancelled.
func Run(ctx context.Context, configPath string, port int) error {
	setupLoggingTo(os.Stdout, slog.LevelInfo)
	var hooks shutdownHooks
	handler, cleanup, addr, err := build(ctx, configPath, port, true, &hooks)
	if err != nil {
		return err
	}
	defer cleanup()

	if pprofAddr := os.Getenv("QUACK_PPROF_ADDR"); pprofAddr != "" {
		go func() {
			slog.Warn("pprof debug endpoint enabled", "component", "serve", "addr", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil && err != http.ErrServerClosed {
				slog.Error("pprof serve failed", "component", "serve", "err", err)
			}
		}()
	}

	srv := &http.Server{Addr: addr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("quack listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()
	select {
	case err := <-serveErr:
		return fmt.Errorf("http serve failed: %w", err)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	// Before srv.Shutdown: DrainActiveRuns' first act is Hub.BeginDraining, so
	// a request landing in this window is rejected, not started unattended.
	if hooks.hub != nil {
		DrainActiveRuns(hooks.hub, hooks.pauser, hooks.grace)
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	slog.Info("stopped")
	return nil
}

// InProcess builds the server on an ephemeral loopback port for co-hosted CLI use.
func InProcess(ctx context.Context, configPath string) (baseURL string, stop func() error, err error) {
	cfg, err := config.LoadDeferringAgentCompleteness(configPath)
	if err != nil {
		return "", nil, fmt.Errorf("config load failed: %w", err)
	}
	return InProcessFromConfig(ctx, cfg)
}

// InProcessFromConfig is InProcess for a caller with a resolved *config.Config in memory.
func InProcessFromConfig(ctx context.Context, cfg *config.Config) (baseURL string, stop func() error, err error) {
	return InProcessWithPromptSource(ctx, cfg, nil)
}

// InProcessWithPromptSource is InProcessFromConfig with a prompt Source that wins over
// the configured store (an experiment pinning `system/<agent>@N`); nil means the store.
func InProcessWithPromptSource(ctx context.Context, cfg *config.Config, promptSrc artifactsrc.Source) (baseURL string, stop func() error, err error) {
	setupLoggingTo(os.Stderr, slog.LevelWarn)
	openaimodel.SetInProcess() // the CLI already reports a model failure; skip the duplicate boundary log
	handler, cleanup, _, err := buildFromConfig(ctx, cfg, 0, false, nil, promptSrc)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("listen loopback: %w", err)
	}
	srv := &http.Server{Handler: handler}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("in-process serve failed", "component", "serve", "err", err)
		}
	}()
	stop = func() error {
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		serr := srv.Shutdown(sc)
		cleanup()
		return serr
	}
	return "http://" + ln.Addr().String(), stop, nil
}

// shutdownHooks is an out-param for what Run needs to drain SIGTERM, avoiding a wider buildFromConfig
// return. nil skips draining (InProcess's CLI use).
type shutdownHooks struct {
	hub *stream.Hub
	// pauser: the drain pauses running nodes rather than cancelling the runs.
	pauser nodePauser
	grace  time.Duration
}

// build loads config and constructs the HTTP handler, shared by Run and InProcess.
func build(ctx context.Context, configPath string, port int, reconcile bool, hooks *shutdownHooks) (handler http.Handler, cleanup func(), addr string, err error) {
	cfg, err := config.LoadDeferringAgentCompleteness(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("config load failed: %w", err)
	}
	return buildFromConfig(ctx, cfg, port, reconcile, hooks, nil)
}

// buildFromConfig is build for an already-loaded Config. reconcile gates startup orphan reconciliation.
// boot carries the shared startup state between buildFromConfig's stages.
type boot struct {
	cfg *config.Config
	// res resolves the named prompt artifacts; nil Source (no prompts: block)
	// means every resolve is the shipped file.
	res *artifactsrc.Resolver
	// admission is the capacity ledger every model client for a registered
	// model shares, not just the orchestrator's.
	admission *dag.Admission
	hooks     *shutdownHooks
	cleanups  []func()
	// decisions is built after the extensions, whose declared points it validates.
	decisions *decide.Decider
	// pristine is cfg before plugin seeding; a reload seeds a fresh copy of it.
	pristine   *config.Config
	seedOwners map[string]string // plugin-seeded agent -> plugin
}

func (b *boot) runCleanups() {
	for i := len(b.cleanups) - 1; i >= 0; i-- {
		b.cleanups[i]()
	}
}

// initAuthAndObservability builds auth, then the ledger store, then starts otel.
func (b *boot) initAuthAndObservability(ctx context.Context) (*auth.Auth, ledger.LedgerStore, error) {
	authMW, err := auth.New(b.cfg.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("auth init failed: %w", err)
	}
	ledgerStore := LedgerStoreFromConfig(b.cfg)
	if err := b.initObservability(ctx, ledgerStore); err != nil {
		return nil, nil, err
	}
	return authMW, ledgerStore, nil
}

// initializes otel, wiring its shutdown (with a bounded context) into the boot cleanups
func (b *boot) initObservability(ctx context.Context, ledgerStore ledger.LedgerStore) error {
	inference.Version = Version // llm.call ledger provenance
	_, otelShutdown, err := otelobs.Init(ctx, b.cfg.Observability, ledgerStore, Version)
	if err != nil {
		return fmt.Errorf("otel init failed: %w", err)
	}
	b.cleanups = append(b.cleanups, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(sctx); err != nil {
			slog.Warn("otel shutdown failed", "component", "otelobs", "err", err)
		}
	})
	slog.SetDefault(slog.New(otelobs.WrapHandler(slog.Default().Handler())))
	otelobs.SetCaptureContent(b.cfg.Observability.Otel.Content)
	if b.cfg.Observability.Otel.IsEnabled() && len(b.cfg.Observability.Otel.Exporters) > 0 && !b.cfg.Observability.Otel.Content {
		slog.Info("span content capture is off (observability.otel.capture_content) - generation spans export with no prompt/response text", "component", "otelobs")
	}
	return nil
}

// creates the workspace jail; managed servers also bring up their store containers
func (b *boot) initWorkspace(ctx context.Context) (*workspace.Jail, error) {
	workspace.ConfineGit(true) // before the plugin registry's boot fetch, the first git quack runs
	jail, err := workspace.NewJail(b.cfg.Workspace.Root)
	if err != nil {
		return nil, fmt.Errorf("workspace init failed: %w", err)
	}
	if b.cfg.Server.Managed() {
		if err = upStores(ctx); err != nil {
			return nil, err
		}
		b.cleanups = append(b.cleanups, func() {
			slog.Info("managed stores left running; tear down with `docker compose -p quack-stores down`", "component", "serve")
		})
	}
	return jail, nil
}

// opens the session store and artifact service, reconciling resumable nodes
func (b *boot) initStorage(ctx context.Context, reconcile bool, jail *workspace.Jail, ledgerStore ledger.LedgerStore) (*store.Store, []store.ResumableNode, *store.TurnAwareService, error) {
	sessionStore, ok := b.cfg.Store(b.cfg.Session.Store)
	if !ok {
		return nil, nil, nil, fmt.Errorf("session store %q not found in stores registry", b.cfg.Session.Store)
	}
	st, err := store.New(sessionStore.Kind, sessionStore.URL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store open failed: %w", err)
	}
	artifactSvc, err := BuildArtifactService(b.cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("artifact service init failed: %w", err)
	}
	artifacts := store.NewTurnAwareService(artifactSvc)
	st.SetArtifactService(artifacts)
	if ledgerStore != nil {
		// Chat/turn/plan writes go through AppendIntent; wired before the reconcile so its settles reach the ledger.
		st.SetWALLedger(ledgerStore)
	}
	// After SetArtifactService: the reconcile mirrors each node's row status onto its dag_node record.
	resumeNodes, err := bootReconcile(reconcile, b.cfg, st, jail)
	if err != nil {
		return nil, nil, nil, err
	}
	warnIfEpisodicRecordsWontSurvive(b.cfg)
	return st, resumeNodes, artifacts, nil
}

// builds the orchestrator model
func (b *boot) initOrchestratorModel(artifacts artifact.Service) (model.LLM, error) {
	prov, _ := b.cfg.Provider(b.cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, b.cfg.Orchestrator.Model, artifacts, b.cfg.ModelCost(b.cfg.Orchestrator.Model), b.cfg.ModelEffort(b.cfg.Orchestrator.Model))
	if err != nil {
		return nil, fmt.Errorf("inference model init failed: %w", err)
	}
	// Never runs inside a DAG node, so no vetting.Config.Agent stamps it.
	setDefaultAgent(llm, orchestrator.AgentName)
	return llm, nil
}

// skillsInit is initSkills' result, including the reloader REST plugin changes call.
type skillsInit struct {
	plugins          []plugin.Plugin
	builtinSkillSrc  skill.Source
	skillSrc         skill.Source
	skillTS          *skilltoolset.SkillToolset
	newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error)
	// reload re-admits the registry with the same per-row admission as boot; it rebuilds agents too once
	// boot attaches them.
	reload *reloader
	// mcpDeclared reports, by row name, which plugins' mcp.json declares a server.
	mcpDeclared func() map[string]bool
	// reg is the SAME registry bootPluginRegistry opened - initHTTP/mountHTTP
	// reuse it instead of opening a second connection to a DB-backed store.
	reg pluginreg.FetchRegistry
}

// resolvePlugins resolves and admits every registry row before rawShapes/buildAgents, so plugin
// agents/shapes seed into cfg first. st (nilable) reuses the registry's DB connection.
func (b *boot) resolvePlugins(ctx context.Context, st *store.Store) (pluginreg.FetchRegistry, []pluginreg.Plugin, []plugin.Plugin, error) {
	reg, rows, err := b.bootPluginRegistry(ctx, st)
	if err != nil {
		return nil, nil, nil, err
	}
	// admitPlugins fails boot only on a plugins.seed refusal; a REST-added row's refusal drops that plugin.
	plugins, err := resolveRegistryPlugins(b.cfg.Plugins.Root, rows)
	if err != nil {
		return nil, nil, nil, err
	}
	plugins, _, err = admitPlugins(ctx, reg, rows, plugins, b.cfg.Plugins.Seed, b.cfg.Extensions.Modules)
	if err != nil {
		return nil, nil, nil, err
	}
	return reg, rows, plugins, nil
}

// resolveAndSeedPlugins seeds plugin agents/shapes into b.cfg and returns the raw catalog shapes, both
// before rawShapes/buildAgents run.
func (b *boot) resolveAndSeedPlugins(ctx context.Context, st *store.Store) (pluginreg.FetchRegistry, []plugin.Plugin, []workflowcatalog.Shape, error) {
	reg, _, plugins, err := b.resolvePlugins(ctx, st)
	if err != nil {
		return nil, nil, nil, err
	}
	// Same split as admitPlugins: a plugins.seed row that fails to seed fails
	// boot, a REST-added one is dropped like a reload drops it.
	seedNames := seedPluginNames(b.cfg.Plugins.Seed)
	b.pristine = cloneForSeeding(b.cfg)
	cand, plugins, seedResults, _, err := seedPlugins(b.pristine, plugins, func(p plugin.Plugin, err error) error {
		if seedNames[p.Name] {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	b.cfg.Agents, b.cfg.Workflows = cand.Agents, cand.Workflows
	b.seedOwners = seedOwners(seedResults)
	logPluginSeeds(seedResults)
	// Every agent needs a bundle/model once plugins are merged; the check was deferred so a plugin
	// override could supply them.
	if err := b.cfg.RequireAgentBundlesAndModels(); err != nil {
		return nil, nil, nil, err
	}
	return reg, plugins, workflowcatalog.FromConfig(b.cfg.Workflows, b.cfg.Revision), nil
}

// buildSkillsInit builds the skill sources and toolsets over an already-resolved plugin set.
func (b *boot) buildSkillsInit(jail *workspace.Jail, reg pluginreg.FetchRegistry, plugins []plugin.Plugin, shapesRef *atomic.Pointer[[]workflowcatalog.Shape]) (skillsInit, error) {
	// Every consumer holds this same instance, so a reload's Swap reaches native agents' next round.
	swappable := newSwappableSkillSource(newSkillSource(plugins))
	reload := &reloader{cfg: b.cfg, reg: reg, skills: swappable}
	reload.declared.Store(mcpDeclaredNames(plugins))
	// WrapRef re-reads shapesRef per call, so a later Store of the catalog
	// reaches this already-built Source and everything wrapping it.
	builtinSkillSrc := workflowcatalog.WrapRef(skill.Source(swappable), shapesRef)
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		return skillsInit{}, fmt.Errorf("skills toolset init failed: %w", err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}
	return skillsInit{
		plugins: plugins, builtinSkillSrc: builtinSkillSrc, skillSrc: skillSrc,
		skillTS: skillTS, newScopedSkillTS: newScopedSkillTS, reload: reload,
		mcpDeclared: reload.mcpDeclared, reg: reg,
	}, nil
}

// opens the task/user memory stores and the shared boot event log
func (b *boot) initMemory(ctx context.Context, st *store.Store, artifacts artifact.Service) (*memory.Store, *memory.Store, []func(), *runlog.EventLog, error) {
	taskStore, userStore, startSweeps, err := openMemoryStores(ctx, b.cfg, st, artifacts, b.admission)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// One EventLog shared by boot resume and SDK dispatch (per-run logs leak drain
	// goroutines; EventLog has no Close). REST keeps its own so a boot backlog never delays a live run.
	bootEventLog := runlog.NewEventLog(st)
	return taskStore, userStore, startSweeps, bootEventLog, nil
}

// builds the SDK extensions, their tools, and the ledger recovery path
func (b *boot) initExtensions(ctx context.Context, st *store.Store, runHub *stream.Hub, bootEventLog *runlog.EventLog, orchRef *atomic.Pointer[orchestrator.Orchestrator], artifacts *store.TurnAwareService, jail *workspace.Jail, judgeModelRef *atomic.Pointer[model.LLM], taskStore, userStore *memory.Store, ledgerStore ledger.LedgerStore, shapesRef *atomic.Pointer[[]workflowcatalog.Shape]) ([]builtSDKExtension, []extTool, tools.GitTokenSource, vetting.DeliverFunc, tools.AssignmentFreshnessFunc, tools.AssignmentMetaFunc, *artifactschema.Registry, error) {
	// Built after taskStore/userStore so UpdateChatOrigin's memory-outcome mapping closes over the
	// concrete stores.
	decisions := &extDecisions{}
	sdkExts, err := buildSDKExtensions(b.cfg, st, runHub, bootEventLog, orchRef, artifacts, jail, judgeModelRef, taskStore, userStore, ledgerStore, shapesRef, decisions)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	if b.decisions, err = decisions.build(b.cfg.Decisions); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	artifactSchemas, err := buildArtifactSchemas(sdkExts)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	extTools := sdkExtensionTools(sdkExts)
	// The SDK inverse interfaces' first real consumer: whichever configured module implements them
	// (github, today) supplies quack's push credential and delivery target, not hardcoded to one extension.
	gitTokenSource, deliver, assignmentFreshness, assignmentMeta := discoverSDKToolSources(sdkExts)
	if ledgerStore != nil {
		// Seed a caught-up watermark for any chat that already has a projection's data, before the first
		// watermark-gated write. Idempotent; runs every boot.
		if err := st.SeedProjectionWatermarks(ctx, ledgerStore); err != nil {
			slog.Warn("projection watermark seeding failed; a first-time fold may re-derive history for old chats", "component", "startup", "err", err)
		}
		// Boot-time recovery: settle intents whose projection write never
		// happened (a crash between WAL append and row write) before any run starts.
		proj := cli.Projections{ArtifactRowExists: cli.ArtifactRowChecker(st, artifacts), ChatExists: st.ChatExists}
		proj.DeliveryRecorded, proj.RecordDelivery = vetting.DeliveryProjections(artifacts, ledgerStore, st.SessionUserForChat)
		proj.Delivery, _ = findRecoverer(sdkExts)
		// Fail-open on purpose: a recovery bug must not crash-loop the deploy; the gauge and this Warn are the signal.
		if _, err := cli.Recover(ctx, ledgerStore, nil, proj, false); err != nil {
			slog.Warn("ledger recovery failed; unresolved intents stay unresolved", "component", "startup", "err", err)
		}
	}
	return sdkExts, extTools, gitTokenSource, deliver, assignmentFreshness, assignmentMeta, artifactSchemas, nil
}

// startPluginMCP spawns boot's plugin MCP servers: the portable half of the
// extension tool surface, out of process and jailed. false = no caps, none spawn.
func (b *boot) startPluginMCP(ctx context.Context, jail *workspace.Jail, plugins []plugin.Plugin) (*mcpSet, bool) {
	caps, err := pluginSpawnCaps(b.cfg, jail)
	if err != nil {
		slog.Warn("plugin MCP servers skipped; sandbox caps unavailable", "component", "startup", "err", err)
		return newMCPSet(b.cfg.Workspace.Root, caps), false
	}
	return b.bootPluginMCP(ctx, plugins, caps), true
}

// builds the configured agents (and their gate config, executor lookups, and classify model)
func (b *boot) initAgents(st *store.Store, skillTS *skilltoolset.SkillToolset, builtinSkillSrc skill.Source, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), taskStore *memory.Store, jail *workspace.Jail, gitTokenSource tools.GitTokenSource, extTools []extTool, deliver vetting.DeliverFunc, artifacts artifact.Service, ledgerStore ledger.LedgerStore, judgeModelRef *atomic.Pointer[model.LLM], reg pluginreg.FetchRegistry, artifactSchemas *artifactschema.Registry) (builtAgents, vetting.JudgeFactory, vetting.PlanJudge, *atomic.Pointer[dag.Executor], dag.SetupFunc, func(*config.Config, []extTool) (builtAgents, error), error) {
	var executorRef atomic.Pointer[dag.Executor]
	nodeCancelled := func(chatID, nodeID string) bool {
		ex := executorRef.Load()
		return ex != nil && ex.NodeCancelled(chatID, nodeID)
	}
	repeatGuardTripped := func(chatID, nodeID, msg string) bool {
		ex := executorRef.Load()
		return ex != nil && ex.RepeatGuardTripped(chatID, nodeID, msg)
	}
	registerLiveSteer := func(chatID, nodeID string, f func(string) bool) {
		if ex := executorRef.Load(); ex != nil {
			ex.SetNodeLiveSteer(chatID, nodeID, f)
		}
	}
	unregisterLiveSteer := func(chatID, nodeID string) {
		if ex := executorRef.Load(); ex != nil {
			ex.ClearNodeLiveSteer(chatID, nodeID)
		}
	}
	registerRoundAbort := func(chatID, nodeID string, cancel context.CancelFunc) {
		if ex := executorRef.Load(); ex != nil {
			ex.SetNodeRoundAbort(chatID, nodeID, cancel)
		}
	}
	unregisterRoundAbort := func(chatID, nodeID string) {
		if ex := executorRef.Load(); ex != nil {
			ex.ClearNodeRoundAbort(chatID, nodeID)
		}
	}
	var setupFn dag.SetupFunc
	// Shared by every generation, so shutdown's closeAll also reaches nodes of a retired roster.
	nodeServers := newPerNodeServers()
	b.cleanups = append(b.cleanups, nodeServers.closeAll)
	build := func(cfg *config.Config, tools []extTool, setupOut *dag.SetupFunc) (built builtAgents, judgeFactory vetting.JudgeFactory, planJudge vetting.PlanJudge, judgeModel model.LLM, err error) {
		built.dropped = map[string]error{}
		built.clients, built.models, _, judgeFactory, planJudge, built.gateCfgs, judgeModel, err = buildAgents(cfg, b.res, st.Sessions, skillTS, builtinSkillSrc, newScopedSkillTS, taskStore, jail, gitTokenSource, tools, deliver, nodeCancelled, repeatGuardTripped, registerLiveSteer, unregisterLiveSteer, registerRoundAbort, unregisterRoundAbort, setupOut, artifacts, ledgerStore, reg, b.admission, artifactSchemas, nodeServers, built.dropped)
		return built, judgeFactory, planJudge, judgeModel, err
	}
	built, judgeFactory, planJudge, judgeModel, err := build(b.cfg, extTools, &setupFn)
	if err != nil {
		return builtAgents{}, nil, nil, nil, nil, nil, fmt.Errorf("agent build failed: %w", err)
	}
	if judgeModel != nil {
		// Classify is not a node, but gated nodes stamp judgeModel; give it an unstamped instance.
		classifyModel := judgeModel
		if jprov, ok := b.cfg.Provider(b.cfg.Gates.Judge.Provider); ok {
			if m, err := inference.NewModel(jprov, b.cfg.Gates.Judge.Model, artifacts, b.cfg.ModelCost(b.cfg.Gates.Judge.Model), b.cfg.ModelEffort(b.cfg.Gates.Judge.Model)); err == nil {
				classifyModel = m
			} else {
				slog.Warn("classify: own judge model unavailable; sharing the gate's (attribution may follow another node)",
					"component", "startup", "err", err)
			}
		}
		// Classify fires outside any DAG node's round, so it must reserve its own capacity.
		classifyModel = dag.NewAdmittingLLM(classifyModel, b.admission, lightweightSpec(b.cfg, b.cfg.Gates.Judge.Model), nil, nil)
		judgeModelRef.Store(&classifyModel)
	}
	// A reload rebuilds agents the same way, minus the boot-fixed judge and setup.
	rebuild := func(cfg *config.Config, tools []extTool) (builtAgents, error) {
		built, _, _, _, err := build(cfg, tools, nil)
		return built, err
	}
	return built, judgeFactory, planJudge, &executorRef, setupFn, rebuild, nil
}

// assembles the orchestrator, re-enters resumed nodes, and starts the extensions and sweeps
func (b *boot) initOrchestrator(ctx context.Context, st *store.Store, llm model.LLM, roster *dag.Roster, judgeFactory vetting.JudgeFactory, planJudge vetting.PlanJudge, taskStore, userStore *memory.Store, artifacts artifact.Service, ledgerStore ledger.LedgerStore, assignmentFreshness tools.AssignmentFreshnessFunc, assignmentMeta tools.AssignmentMetaFunc, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), skillSrc skill.Source, orchRef *atomic.Pointer[orchestrator.Orchestrator], resumeNodes []store.ResumableNode, runHub *stream.Hub, bootEventLog *runlog.EventLog, sdkExts []builtSDKExtension, startSweeps []func(), hooks *shutdownHooks, executorRef *atomic.Pointer[dag.Executor], setupFn dag.SetupFunc, artifactSchemas *artifactschema.Registry) (*orchestrator.Orchestrator, error) {
	orch, err := assembleOrchestrator(ctx, b.cfg, b.res, st, llm, roster, judgeFactory, planJudge, b.decisions, taskStore, userStore, artifacts, ledgerStore, assignmentFreshness, assignmentMeta, newScopedSkillTS, skillSrc, orchRef, executorRef, hooks, setupFn, b.admission, artifactSchemas)
	if err != nil {
		return nil, err
	}
	// Re-enter each resumed node's graph only after the orchestrator exists:
	// a crash here leaves them paused; the next boot picks them up (reconcile already ran).
	startResumedNodes(ctx, resumeNodes, orch, st, runHub, bootEventLog, bootResumeConcurrency)
	go syncFinishedNodeRecords(ctx, st)
	for _, start := range startSweeps {
		start()
	}
	if err := startSDKExtensions(ctx, sdkExts); err != nil {
		return nil, fmt.Errorf("sdk extensions start failed: %w", err)
	}
	if userStore != nil && b.cfg.Orchestrator.UserMemoryHook.Enabled {
		if memAgent, err := buildUserMemoryHookAgent(ctx, b.cfg.Orchestrator.UserMemoryHook, b.cfg, b.res, artifacts, b.admission); err != nil {
			slog.Warn("user memory hook build failed; hook disabled", "component", "startup", "err", err)
		} else {
			orch.SetUserMemoryHook(memAgent)
			slog.Info("user memory hook enabled", "component", "startup", "model", b.cfg.Orchestrator.UserMemoryHook.Model)
		}
	}
	return orch, nil
}

// mounts the HTTP handler and starts the workspace GC
func (b *boot) initHTTP(ctx context.Context, st *store.Store, orch *orchestrator.Orchestrator, llm model.LLM, jail *workspace.Jail, runHub *stream.Hub, ledgerStore ledger.LedgerStore, taskStore, userStore *memory.Store, artifacts *store.TurnAwareService, sdkExts []builtSDKExtension, authMW *auth.Auth, reload *reloader, pluginReg pluginreg.FetchRegistry) (http.Handler, error) {
	handler, err := mountHTTP(b.cfg, st, orch, llm, jail, runHub, ledgerStore, taskStore, userStore, artifacts, sdkExts, authMW, reload, pluginReg)
	if err != nil {
		return nil, err
	}
	if err := startWorkspaceGC(ctx, b.cfg, jail, runHub); err != nil {
		return nil, err
	}
	return handler, nil
}

func buildFromConfig(ctx context.Context, cfg *config.Config, port int, reconcile bool, hooks *shutdownHooks, promptOverride artifactsrc.Source) (handler http.Handler, cleanup func(), addr string, err error) {
	promptSrc, promptSrcName := promptSourceFor(cfg, promptOverride)
	b := &boot{cfg: cfg, res: artifactsrc.New(promptSrcName, promptSrc, cfg.Prompts.CacheTTLDuration()), admission: buildAdmission(cfg), hooks: hooks}
	defer func() {
		if err != nil {
			b.runCleanups()
			handler = nil
		}
	}()
	seedPromptArtifacts(ctx, promptSrc)

	// Pinned ACP processes close on node-finish and on shutdown (vetting can't import acp, hence the hook).
	vetting.NodeSessionClosed = acp.ClosePinnedSession
	b.cleanups = append(b.cleanups, acp.CloseAllPinnedSessions)
	promptbuilder.SetLocation(cfg.Location())
	logTimezone(cfg)

	addr = cfg.Server.Addr
	if port != 0 {
		addr = fmt.Sprintf(":%d", port)
	}

	authMW, ledgerStore, err := b.initAuthAndObservability(ctx)
	if err != nil {
		return nil, nil, "", err
	}

	// Chat hard-delete is the only ledger GC; checkpoints bound fold cost instead of trimming the log.
	jail, err := b.initWorkspace(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	st, resumeNodes, artifacts, err := b.initStorage(ctx, reconcile, jail, ledgerStore)
	if err != nil {
		return nil, nil, "", err
	}
	llm, err := b.initOrchestratorModel(artifacts)
	if err != nil {
		return nil, nil, "", err
	}

	// runHub is needed by the SDK extensions built below (Dispatch fans a
	// run's events through it) as well as REST.
	runHub := stream.NewHub()
	if hooks != nil {
		hooks.hub = runHub
		hooks.grace = time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	}

	// orchRef/judgeModelRef resolve further down: SDK Dispatch/Classify may fire long after
	// construction, but their Tools() are needed now to fold into extTools before buildAgents.
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	var judgeModelRef atomic.Pointer[model.LLM]

	reg, plugins, rawShapes, err := b.resolveAndSeedPlugins(ctx, st)
	if err != nil {
		return nil, nil, "", err
	}
	// Both catalog consumers below are built before buildAgents runs, so this
	// starts unfiltered; the first generation's catalogShapes corrects it once buildAgents returns.
	var shapesRef atomic.Pointer[[]workflowcatalog.Shape]
	shapesRef.Store(&rawShapes)

	skills, err := b.buildSkillsInit(jail, reg, plugins, &shapesRef)
	if err != nil {
		return nil, nil, "", err
	}
	taskStore, userStore, startSweeps, bootEventLog, err := b.initMemory(ctx, st, artifacts)
	if err != nil {
		return nil, nil, "", err
	}
	sdkExts, sdkTools, gitTokenSource, deliver, assignmentFreshness, assignmentMeta, artifactSchemas, err := b.initExtensions(ctx, st, runHub, bootEventLog, &orchRef, artifacts, jail, &judgeModelRef, taskStore, userStore, ledgerStore, &shapesRef)
	if err != nil {
		return nil, nil, "", err
	}
	mcp, mcpOn := b.startPluginMCP(ctx, jail, skills.plugins)
	built, judgeFactory, planJudge, executorRef, setupFn, rebuild, err := b.initAgents(st, skills.skillTS, skills.builtinSkillSrc, skills.newScopedSkillTS, taskStore, jail, gitTokenSource, append(slices.Clone(sdkTools), mcp.tools()...), deliver, artifacts, ledgerStore, &judgeModelRef, skills.reg, artifactSchemas)
	if err != nil {
		return nil, nil, "", err
	}
	rr := &rosterReload{pristine: b.pristine, res: b.res, build: rebuild, sdkTools: sdkTools, mcpOn: mcpOn, shapesRef: &shapesRef}
	rr.cur = rr.generation(ctx, b.cfg, built, mcp, b.seedOwners, 1)
	shapesRef.Store(&rr.cur.shapes)
	orch, err := b.initOrchestrator(ctx, st, llm, rr.cur.roster, judgeFactory, planJudge, taskStore, userStore, artifacts, ledgerStore, assignmentFreshness, assignmentMeta, skills.newScopedSkillTS, skills.skillSrc, &orchRef, resumeNodes, runHub, bootEventLog, sdkExts, startSweeps, hooks, executorRef, setupFn, artifactSchemas)
	if err != nil {
		return nil, nil, "", err
	}
	rr.exec = executorRef.Load()
	skills.reload.attach(rr)
	b.cleanups = append(b.cleanups, skills.reload.shutdown)
	handler, err = b.initHTTP(ctx, st, orch, llm, jail, runHub, ledgerStore, taskStore, userStore, artifacts, sdkExts, authMW, skills.reload, skills.reg)
	if err != nil {
		return nil, nil, "", err
	}
	return handler, b.runCleanups, addr, nil
}

// promptSourceFor: the live store Source, chained behind a caller-supplied override (an experiment's pins).
func promptSourceFor(cfg *config.Config, override artifactsrc.Source) (artifactsrc.Source, string) {
	src, name := buildPromptSource(cfg)
	if override != nil {
		// Chained, not swapped: a name the override doesn't pin (e.g. an experiment
		// pinning only its own agent) still resolves off the configured store.
		return artifactsrc.Chain(override, src), name
	}
	return src, name
}

// buildPromptSource builds the prompts: store's Source and its stores: name; (nil, "")
// when prompts: names no store, so New falls back to the static-only Resolver.
func buildPromptSource(cfg *config.Config) (artifactsrc.Source, string) {
	if cfg.Prompts.Store == "" {
		return nil, ""
	}
	// validatePrompts already checked the store exists, is kind langfuse, and has both keys.
	sc, _ := cfg.Store(cfg.Prompts.Store)
	client := langfuse.New(sc.URL, sc.PublicKey, sc.SecretKey, langfuse.WithPinLabel(cfg.Prompts.Label()))
	return &langfuse.Source{Client: client, StoreKey: cfg.Prompts.Store}, cfg.Prompts.Store
}

// seedPromptArtifacts pushes shipped artifacts to src in the background: seeding must never delay
// readiness, and a failed name keeps the store's version (the resolver falls back to static anyway).
func seedPromptArtifacts(ctx context.Context, src artifactsrc.Source) {
	if src == nil {
		return
	}
	go func() {
		for _, name := range artifactsrc.Names() {
			if ctx.Err() != nil {
				return
			}
			static, err := artifactsrc.Static(name)
			if err != nil {
				continue // scan() already logged a missing shipped artifact
			}
			if err := src.Seed(ctx, name, static); err != nil {
				slog.Warn("prompt seed failed", "component", "artifacts", "artifact", name, "err", err)
			}
		}
	}()
}

// setupLoggingTo installs the process-wide slog handler from QUACK_LOG_LEVEL / QUACK_LOG_FORMAT.
func setupLoggingTo(w io.Writer, fallback slog.Level) {
	lvl := fallback
	if s := os.Getenv("QUACK_LOG_LEVEL"); s != "" {
		_ = lvl.UnmarshalText([]byte(s))
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if strings.EqualFold(os.Getenv("QUACK_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
}

// buildUserMemoryHookAgent builds the memory-extraction agent from the memory-agent bundle.
func buildUserMemoryHookAgent(ctx context.Context, h config.UserMemoryHookConfig, cfg *config.Config, res *artifactsrc.Resolver, artifacts artifact.Service, admission *dag.Admission) (adkagent.Agent, error) {
	prov, ok := cfg.Provider(h.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q not found", h.Provider)
	}
	m, err := inference.NewModel(prov, h.Model, artifacts, cfg.ModelCost(h.Model), cfg.ModelEffort(h.Model))
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	// Fires from a fire-and-forget goroutine after the orchestrator's own turn ends,
	// via its own nested runner.Run - never a DAG node's own model call.
	setDefaultAgent(m, "memory-hook")
	var wm model.LLM = dag.NewAdmittingLLM(m, admission, lightweightSpec(cfg, h.Model), nil, nil)
	b, err := agent.LoadBundle(ctx, res, "agents/memory-agent")
	if err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	whatToRemember, _, err := agent.LoadBundleMemory(ctx, res, "agents/orchestrator")
	if err != nil {
		return nil, fmt.Errorf("orchestrator memory.md: %w", err)
	}
	rubric, err := vetting.LoadBundleRubric(ctx, res, "agents/memory-agent")
	if err != nil {
		return nil, fmt.Errorf("rubric.md: %w", err)
	}
	guidance := strings.TrimSpace(whatToRemember + "\n\n" + rubric)
	return agent.BuildChat(b, b.PinPrompt(res), wm, nil, nil, guidance, "")
}

// fetchGitCredential: shared body of the two GitCredential adapters - resolve, nil-check, copy fields.
func fetchGitCredential[C, D any](ctx context.Context, rawURL string, fetch func(context.Context, string) (*C, error), fields func(*C) (string, string, string), mk func(string, string, string) D) (*D, error) {
	c, err := fetch(ctx, rawURL)
	if err != nil || c == nil {
		return nil, err
	}
	h, u, t := fields(c)
	d := mk(h, u, t)
	return &d, nil
}

// gitCredentialAdapter bridges tools.GitTokenSource to vetting.GitCredentialSource -
// vetting can't import internal/tools (tools already imports vetting), so it declares its own type.
type gitCredentialAdapter struct{ src tools.GitTokenSource }

func toolsCredFields(c *tools.GitCredential) (string, string, string) {
	return c.Host, c.Username, c.Token
}

func newVettingGitCredential(h, u, t string) vetting.GitCredential {
	return vetting.GitCredential{Host: h, Username: u, Token: t}
}

func (a gitCredentialAdapter) GitCredential(ctx context.Context, rawURL string) (*vetting.GitCredential, error) {
	return fetchGitCredential(ctx, rawURL, a.src.GitCredential, toolsCredFields, newVettingGitCredential)
}

// gateConfigs holds each gated agent's boot-resolved gate config plus the closure that re-resolves its
// artifacts at run start, so an edited rubric or prompt applies without a restart.
type gateConfigs struct {
	boot    map[string]vetting.Config
	refresh map[string]func(context.Context) (vetting.Config, error)
}

func newGateConfigs(n int) *gateConfigs {
	return &gateConfigs{boot: make(map[string]vetting.Config, n), refresh: make(map[string]func(context.Context) (vetting.Config, error), n)}
}

// For is dag's cfgFor: the agent's gate config as its artifacts resolve right
// now. An unresolvable artifact keeps the boot config rather than failing the run.
func (g *gateConfigs) For(ctx context.Context, name string) vetting.Config {
	c := g.boot[name]
	if r := g.refresh[name]; r != nil {
		fresh, err := r(ctx)
		if err != nil {
			slog.WarnContext(ctx, "gate artifacts unresolved; using the loaded config", "component", "serve", "agent", name, "err", err)
			return c
		}
		return fresh
	}
	return c
}

// buildAgents loads each agent bundle, builds its model and tools, exposes over A2A, returns client map.
func buildAgents(cfg *config.Config, res *artifactsrc.Resolver, sessions session.Service, skillTS *skilltoolset.SkillToolset, builtinSkillSrc skill.Source, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), taskStore *memory.Store, jail *workspace.Jail, gitTokenSource tools.GitTokenSource, extTools []extTool, deliver vetting.DeliverFunc, nodeCancelled func(chatID, nodeID string) bool, repeatGuardTripped func(chatID, nodeID, msg string) bool, registerLiveSteer func(chatID, nodeID string, f func(string) bool), unregisterLiveSteer func(chatID, nodeID string), registerRoundAbort func(chatID, nodeID string, cancel context.CancelFunc), unregisterRoundAbort func(chatID, nodeID string), setupOut *dag.SetupFunc, artifacts artifact.Service, ledgerStore ledger.LedgerStore, reg pluginreg.FetchRegistry, admission *dag.Admission, artifactSchemas *artifactschema.Registry, nodeServers *perNodeServers, dropped map[string]error) (map[string]adkagent.Agent, map[string]model.LLM, *perNodeServers, vetting.JudgeFactory, vetting.PlanJudge, *gateConfigs, model.LLM, error) {

	names := slices.Sorted(maps.Keys(cfg.Agents))

	urlCache := tools.NewURLCache()

	workspaceCaps := workspace.Caps{
		MaxReadBytes:   int64(cfg.Workspace.MaxReadKB) * 1024,
		MaxWriteBytes:  int64(cfg.Workspace.MaxWriteKB) * 1024,
		MaxResults:     cfg.Workspace.MaxResults,
		MaxListEntries: cfg.Workspace.MaxListEntries,
		Timeout:        time.Duration(cfg.Workspace.TimeoutSeconds) * time.Second,
		ExtraPath:      cfg.Workspace.ExecPath,
		Env:            cfg.Workspace.Env,
		BuildDirs:      cfg.Workspace.BuildDirs,
		Limits: workspace.Limits{
			AddressSpaceMB: cfg.Workspace.Limits.AddressSpaceMB,
			Procs:          cfg.Workspace.Limits.MaxProcs,
			FileSizeMB:     cfg.Workspace.Limits.MaxFileSizeMB,
		},
	}
	sandbox, err := workspace.ResolveSandbox(workspace.SandboxMode(cfg.Workspace.Sandbox))
	if err != nil {
		return nil, nil, nodeServers, nil, nil, nil, nil, err
	}
	workspaceCaps.Sandbox = sandbox
	homeDir, err := jail.HomeDir(localUserID)
	if err != nil {
		return nil, nil, nodeServers, nil, nil, nil, nil, fmt.Errorf("workspace home dir init failed: %w", err)
	}
	workspaceCaps.HomeDir = homeDir

	gateCfg, judgeFactory, planJudge, judgeModel, safetyJudge, err := buildGateJudge(cfg, res, jail, workspaceCaps, taskStore, ledgerStore, skillTS, artifacts, deliver, gitTokenSource, admission, artifactSchemas)
	if err != nil {
		return nil, nil, nodeServers, nil, nil, nil, nil, err
	}

	gitCredentials := make([]tools.GitCredential, len(cfg.Workspace.GitCredentials))
	for i, gc := range cfg.Workspace.GitCredentials {
		gitCredentials[i] = tools.GitCredential{Host: gc.Host, Username: gc.Username, Token: gc.Token}
	}
	if setupOut != nil {
		*setupOut = setupCloneFunc(cfg, jail, workspaceCaps, gitCredentials, gitTokenSource)
	}

	compactionFor, err := buildCompaction(cfg, res, artifacts)
	if err != nil {
		return nil, nil, nodeServers, nil, nil, nil, nil, err
	}
	clientMap := make(map[string]adkagent.Agent, len(cfg.Agents))
	modelMap := make(map[string]model.LLM, len(cfg.Agents))
	gateCfgs := newGateConfigs(len(cfg.Agents))

	// Plugin MCP tool names come from third-party authors, so a collision with an
	// SDK extension tool is plausible; indexExtTools prefixes colliding names.
	extToolsByName := indexExtTools(extTools)

	for _, name := range names {
		ac := cfg.Agents[name]
		prov, m, err := agentModel(cfg, name, ac, artifacts)
		var ag adkagent.Agent
		if err == nil && ac.Acp != nil {
			ag, err = buildACPNode(name, ac, prov, cfg, res, workspaceCaps, jail, taskStore, builtinSkillSrc, gateCfg, gateCfgs, safetyJudge, cfg.ModelCost(ac.Model), registerLiveSteer, unregisterLiveSteer, registerRoundAbort, unregisterRoundAbort, reg)
		} else if err == nil {
			ag, err = buildNativeNode(name, ac, prov, taskStore, newScopedSkillTS, builtinSkillSrc, cfg, res, workspaceCaps, jail, safetyJudge, nodeCancelled, repeatGuardTripped, extToolsByName, urlCache, sessions, artifacts, ledgerStore, compactionFor, gateCfg, gateCfgs, nodeServers, reg, admission)
		}
		if err != nil {
			if !dropOptionalAgent(name, ac, err, dropped) {
				return nil, nil, nodeServers, nil, nil, nil, nil, err
			}
			continue
		}
		clientMap[name] = ag
		if ac.Acp != nil {
			modelMap[name] = m
			slog.Info("agent running via ACP subprocess", "component", "startup",
				"agent", name, "command", strings.Join(ac.Acp.Command, " "), "model", ac.Model)
		}
	}
	return clientMap, modelMap, nodeServers, judgeFactory, planJudge, gateCfgs, judgeModel, nil
}

// agentModel resolves one agent's provider and builds its model.
func agentModel(cfg *config.Config, name string, ac config.AgentConfig, artifacts artifact.Service) (config.ProviderConfig, model.LLM, error) {
	prov, ok := cfg.Provider(ac.Provider)
	if !ok {
		return prov, nil, fmtErr(name, "provider %q not found", ac.Provider)
	}
	m, err := inference.NewModel(prov, ac.Model, artifacts, cfg.ModelCost(ac.Model), cfg.ModelEffort(ac.Model))
	if err != nil {
		return prov, nil, fmtErr(name, "model: %v", err)
	}
	return prov, m, nil
}

// buildGateJudge assembles the trust-gate config and judge models when the gate is enabled;
// zero values and nil stand in for the disabled case.
func buildGateJudge(cfg *config.Config, res *artifactsrc.Resolver, jail *workspace.Jail, workspaceCaps workspace.Caps, taskStore *memory.Store, ledgerStore ledger.LedgerStore, skillTS *skilltoolset.SkillToolset, artifacts artifact.Service, deliver vetting.DeliverFunc, gitTokenSource tools.GitTokenSource, admission *dag.Admission, artifactSchemas *artifactschema.Registry) (vetting.Config, vetting.JudgeFactory, vetting.PlanJudge, model.LLM, tools.SafetyJudge, error) {
	var gateCfg vetting.Config
	var judgeFactory vetting.JudgeFactory
	var planJudge vetting.PlanJudge
	var judgeModel model.LLM
	var safetyJudge tools.SafetyJudge
	if cfg.Gates.Enabled() {
		var err error
		if gateCfg, err = vetting.FromConfig(context.Background(), res, cfg.Gates); err != nil {
			return vetting.Config{}, nil, nil, nil, nil, err
		}
		gateCfg.Prompts = res // system/judge, resolved once per judge round
		gateCfg.Memory = taskStore
		gateCfg.Workspace = jail
		gateCfg.WorkspaceUserID = localUserID
		gateCfg.WorkspaceCaps = workspaceCaps
		gateCfg.CheckTimeout = time.Duration(cfg.Workspace.CheckTimeoutSeconds) * time.Second
		gateCfg.Deliver = deliver
		if gitTokenSource != nil {
			gateCfg.GitCredentials = gitCredentialAdapter{gitTokenSource}
		}
		gateCfg.CheckCommands = cfg.Workspace.CheckCommands
		gateCfg.CheckSetup = cfg.Workspace.CheckSetup
		if cfg.Gates.JudgeEnabled() {
			jprov, ok := cfg.Provider(cfg.Gates.Judge.Provider)
			if !ok {
				return vetting.Config{}, nil, nil, nil, nil, fmt.Errorf("gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
			}
			judge, err := inference.NewModel(jprov, cfg.Gates.Judge.Model, artifacts, cfg.ModelCost(cfg.Gates.Judge.Model), cfg.ModelEffort(cfg.Gates.Judge.Model))
			if err != nil {
				return vetting.Config{}, nil, nil, nil, nil, fmt.Errorf("gates.judge: model: %w", err)
			}
			judgeModel = judge
			gateCfg.JudgeModel = judge
			var judgeReadTools []tool.Tool
			if jail != nil {
				judgeReadTools, err = tools.Build([]string{"read_file", "list_dir", "glob", "grep"}, tools.Deps{
					Workspace:       jail,
					WorkspaceUserID: localUserID,
					WorkspaceCaps:   workspaceCaps,
				})
				if err != nil {
					return vetting.Config{}, nil, nil, nil, nil, fmt.Errorf("gates.judge: read tools: %w", err)
				}
			}
			// Unwrapped: judgeSessionID is per chat, so shared repeatStates would refuse a chat's 3rd
			// load_skill(rubric) round; the judge breaks in-round loops itself.
			var judgeSkillsets []tool.Toolset
			if skillTS != nil {
				judgeSkillsets = []tool.Toolset{skillTS}
			}
			judgeFactory = vetting.NewJudgeFactory(judge, judgeReadTools, judgeSkillsets)
			judgeFactoryNoTools := vetting.NewJudgeFactory(judge, nil, judgeSkillsets)
			// Each round gets its own bound factory+model; one shared instance swapped in place let rounds race.
			gateCfg.RefreshJudgeBinding = bindJudgeRefresher(cfg, jprov, artifacts, judge, judgeFactory, judgeFactoryNoTools, judgeReadTools, judgeSkillsets)
			// Own instances: gated nodes stamp per-round coords on `judge`, and these non-node callers would
			// inherit the last node's stamp.
			unstamped := func() (model.LLM, error) {
				return inference.NewModel(jprov, cfg.Gates.Judge.Model, artifacts, cfg.ModelCost(cfg.Gates.Judge.Model), cfg.ModelEffort(cfg.Gates.Judge.Model))
			}
			safetyModel, err := unstamped()
			if err != nil {
				return vetting.Config{}, nil, nil, nil, nil, fmt.Errorf("gates.judge: safety judge model: %w", err)
			}
			planModel, err := unstamped()
			if err != nil {
				return vetting.Config{}, nil, nil, nil, nil, fmt.Errorf("gates.judge: plan judge model: %w", err)
			}
			// Unwrapped: guardedTool.Run and acpPermJudge call this from inside a
			// node's own held reservation - nesting an Admit there deadlocks it.
			safetyJudge = tools.NewSafetyJudge(safetyModel)
			// Wrapped: a plan judge round fires between turns, never inside a held
			// node reservation, so it must reserve its own capacity.
			planJudge = vetting.NewPlanJudge(dag.NewAdmittingLLM(planModel, admission, judgeSpec(cfg), nil, nil), cfg.Gates.Judge.MaxOutputTokens, cfg.Gates.Judge.ThinkingLevel, taskStore, ledgerStore)
		}
		slog.Info("trust gate enabled", "component", "startup",
			"deterministic_rounds", gateCfg.DeterministicRounds,
			"judge", cfg.Gates.Judge.Model, "judge_rounds", gateCfg.JudgeRounds, "threshold", gateCfg.Threshold)
	}
	// Store invariant, not gate policy: armed even with gates disabled, so the
	// native tool builder (nativeNodeBuilder.schemas) is never silently disarmed.
	gateCfg.Schemas = artifactSchemas
	return gateCfg, judgeFactory, planJudge, judgeModel, safetyJudge, nil
}

// judgeBoundModel is one cached (provider, model) judge factory/model pair.
type judgeBoundModel struct {
	factory vetting.JudgeFactory
	model   model.LLM
}

// judgeBinding caches every bound-in (provider, model) pair by that key, guarded
// by mu since its closure is shared by every concurrent gated node's judge round.
type judgeBinding struct {
	mu           sync.Mutex
	lastBad      string
	lastBadBuild string
	cache        map[string]judgeBoundModel
}

// warnOnce logs msg via slog.Warn only the first time this call's err differs
// from *last (deduping a persistent failure to one log per distinct error).
func (b *judgeBinding) warnOnce(last *string, msg string, artifactName string, err error) {
	b.mu.Lock()
	changed := err.Error() != *last
	if changed {
		*last = err.Error()
	}
	b.mu.Unlock()
	if changed {
		slog.Warn(msg, "component", "artifacts", "artifact", artifactName, "err", err)
	}
}

// bindJudgeRefresher: system/judge's Config picks each round's own JudgeFactory+model+thinking_level; an
// invalid value falls back to gates.judge's static binding and logs once per distinct bad value.
func bindJudgeRefresher(cfg *config.Config, jprov config.ProviderConfig, artifacts artifact.Service, staticModel model.LLM, staticFactory, staticFactoryNoTools vetting.JudgeFactory, readTools []tool.Tool, skillsets []tool.Toolset) func(art artifactsrc.Artifact, hasReadTools bool) (vetting.JudgeFactory, model.LLM, string) {
	staticEffort := cfg.Gates.Judge.ThinkingLevel
	b := &judgeBinding{cache: map[string]judgeBoundModel{}}
	return func(art artifactsrc.Artifact, hasReadTools bool) (vetting.JudgeFactory, model.LLM, string) {
		staticForNode := staticFactory
		if !hasReadTools {
			staticForNode = staticFactoryNoTools
		}
		bound, err := cfg.ResolveBinding(jprov, cfg.Gates.Judge.Model, art.Config)
		if err != nil {
			b.warnOnce(&b.lastBad, "judge prompt binding invalid; using gates.judge's static binding", art.Name, err)
			return staticForNode, staticModel, staticEffort
		}
		b.mu.Lock()
		b.lastBad = ""
		b.mu.Unlock()
		if bound == nil {
			return staticForNode, staticModel, staticEffort
		}
		effort := staticEffort
		if e, ok := art.Config["effort"].(string); ok && e != "" {
			effort = e
		}
		// (provider, model) identifies the swap; effort rides thinking_level. hasReadTools rides the key too:
		// two nodes sharing a bound model must not share a factory.
		key := bound.ProviderName + "|" + bound.Provider.Endpoint + "|" + bound.Model
		if hasReadTools {
			key += "|tools"
		}
		b.mu.Lock()
		cached, ok := b.cache[key]
		b.mu.Unlock()
		if ok {
			return cached.factory, cached.model, effort
		}
		m, err := inference.NewModel(bound.Provider, bound.Model, artifacts, cfg.ModelCost(bound.Model), "")
		if err != nil {
			b.warnOnce(&b.lastBadBuild, "judge prompt binding model build failed; using gates.judge's static binding", art.Name, err)
			return staticForNode, staticModel, staticEffort
		}
		nodeReadTools := readTools
		if !hasReadTools {
			nodeReadTools = nil
		}
		f := vetting.NewJudgeFactory(m, nodeReadTools, skillsets)
		b.mu.Lock()
		b.cache[key] = judgeBoundModel{factory: f, model: m}
		b.mu.Unlock()
		return f, m, effort
	}
}

// buildACPNode builds one ACP-harness agent (external CLI child), registering
// its gate config in gateCfgs when the agent is gated.
func buildACPNode(name string, ac config.AgentConfig, prov config.ProviderConfig, cfg *config.Config, res *artifactsrc.Resolver, workspaceCaps workspace.Caps, jail *workspace.Jail, taskStore *memory.Store, builtinSkillSrc skill.Source, gateCfg vetting.Config, gateCfgs *gateConfigs, safetyJudge tools.SafetyJudge, acpPricing *config.ModelPricing, registerLiveSteer func(chatID, nodeID string, f func(string) bool), unregisterLiveSteer func(chatID, nodeID string), registerRoundAbort func(chatID, nodeID string, cancel context.CancelFunc), unregisterRoundAbort func(chatID, nodeID string), reg pluginreg.FetchRegistry) (adkagent.Agent, error) {
	ctx := context.Background()
	bundle, err := agent.LoadBundle(ctx, res, ac.Bundle)
	if err != nil {
		return nil, fmtErr(name, "bundle: %v", err)
	}
	var memGuidance string
	var memArt artifactsrc.Artifact
	if taskStore != nil {
		if memGuidance, memArt, err = agent.LoadBundleMemory(ctx, res, ac.Bundle); err != nil {
			return nil, fmtErr(name, "memory.md: %v", err)
		}
	}
	grading, err := resolveGateCfg(cfg, res, gateCfg, name, ac, taskStore != nil, memGuidance, memArt, bundle, gateCfgs, reg)
	if err != nil {
		return nil, fmtErr(name, "rubric: %v", err)
	}
	skillFms, err := acpSkillFrontmatters(ctx, builtinSkillSrc, ac.Skills)
	if err != nil {
		return nil, fmtErr(name, "skills: %v", err)
	}
	wsBlock := workspace.PromptBlock(workspaceCaps, cfg.Workspace.CheckCommands)
	// Resolved once where the preamble is composed (steerHooks) so nothing swaps it mid-round; a second
	// resolve for PreambleArtifact could fall back differently and disagree.
	var promptArtMu sync.Mutex
	var promptArt artifactsrc.Artifact
	preamble := promptbuilder.CacheByDay(
		func(ctx context.Context) string { return bundle.ResolvePrompt(ctx, res).VersionID },
		func(ctx context.Context) string {
			art := bundle.ResolvePrompt(ctx, res)
			promptArtMu.Lock()
			promptArt = art
			promptArtMu.Unlock()
			behaviour := agent.BehaviourLayer(strings.TrimSpace(art.Body), memGuidance)
			return promptbuilder.Agent(bundle.Card.Name, bundle.Card.Description, skillFms, true, behaviour, grading, wsBlock)
		})
	preambleArtifact := func(context.Context) artifactsrc.Artifact {
		promptArtMu.Lock()
		defer promptArtMu.Unlock()
		return promptArt
	}
	// skill_paths is filled in per spawn (proc.go's mergeSkillPaths), not
	// baked in here - see skillPathsFn below.
	env := piACPEnv(prov, ac, nil, workspaceCaps.Sandbox)
	env = append(env, acpChildEnv(cfg.Workspace.Env, ac.Acp.Env)...)
	skillPathsFn := acpRegistrySkillPaths(cfg, reg)
	extraROFn := acpRegistryExtraRO(cfg)
	pluginsFn := acpRegistryPluginRefs(cfg, reg)
	var permJudge func(ctx context.Context, toolName, title string, input map[string]any) (bool, string)
	if safetyJudge != nil {
		permJudge = acpPermJudge(safetyJudge, name)
	}
	ag, err := acp.New(name, bundle.Card.Description, acp.Options{
		Command:              ac.Acp.Command,
		Env:                  env,
		Caps:                 workspaceCaps,
		SkillPaths:           skillPathsFn,
		ExtraRO:              extraROFn,
		Plugins:              pluginsFn,
		Home:                 workspaceCaps.HomeDir,
		Preamble:             preamble,
		PreambleArtifact:     preambleArtifact,
		MemoryArtifact:       memArt,
		Prompts:              res,
		Jail:                 jail,
		UserID:               localUserID,
		PermissionJudge:      permJudge,
		ModelName:            ac.Model,
		Pricing:              acpPricing,
		RegisterLiveSteer:    registerLiveSteer,
		UnregisterLiveSteer:  unregisterLiveSteer,
		RegisterRoundAbort:   registerRoundAbort,
		UnregisterRoundAbort: unregisterRoundAbort,
		Worktree: func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (string, error) {
			parentDir, err := jail.Resolve(userID, chatID, workspace.NodeDir(parentNodeID))
			if err != nil {
				return "", fmt.Errorf("acp worktree: resolve parent clone: %w", err)
			}
			return tools.SetupWorktree(ctx, jail, userID, chatID, parentDir, workspace.NodeDir(nodeID),
				workspace.WorktreeBranch(nodeID), workspaceCaps, cfg.Workspace.CheckSetup)
		},
	})
	if err != nil {
		return nil, fmtErr(name, "acp: %v", err)
	}
	return ag, nil
}

type nativeNodeBuilder struct {
	name               string
	prov               config.ProviderConfig
	ac                 config.AgentConfig
	artifacts          artifact.Service
	cfg                *config.Config
	admission          *dag.Admission
	toolNames          []string
	urlCache           *tools.URLCache
	sessions           session.Service
	jail               *workspace.Jail
	safetyJudge        tools.SafetyJudge
	nodeCancelled      func(chatID, nodeID string) bool
	repeatGuardTripped func(chatID, nodeID, msg string) bool
	extToolsByName     map[string]tool.Tool
	taskStore          *memory.Store
	memSvc             func(token string) adkmemory.Service // nil: memory off; else the node's own view
	workspaceCaps      workspace.Caps
	memGuidance        string
	bundle             *agent.Bundle
	agentSkillTS       *skilltoolset.SkillToolset
	grading            string
	ledgerStore        ledger.LedgerStore
	compactionFor      func(ac config.AgentConfig, workerModel model.LLM) agent.Compaction
	nodeServers        *perNodeServers
	res                *artifactsrc.Resolver
	schemas            *artifactschema.Registry
}

func (b *nativeNodeBuilder) buildWorker(prompts *artifactsrc.Pinned, drain func() string, rc *recordstore.Client, nodeID string, coords *tools.RoundCoords, sink func(stream.SSEEvent), turnID string, scope tools.CallScope, meter *agent.PromptMeter, extraTools ...tool.Tool) (adkagent.Agent, model.LLM, *inference.OverridableModel, []tool.Tool, error) {
	base, err := inference.NewModel(b.prov, b.ac.Model, b.artifacts, b.cfg.ModelCost(b.ac.Model), b.cfg.ModelEffort(b.ac.Model))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("model: %w", err)
	}
	// Overridable lets a round rebind model/provider/effort without rebuilding the ADK agent, which holds
	// this LLM for its lifetime.
	wm := inference.NewOverridable(base)
	// Admission sits outside the overridable so a round-bound swap still routes through the hold. The slot
	// frees between model calls; tool phases overlap others.
	var wrapped model.LLM = wm
	if b.admission != nil && nodeID != "" {
		// sink may only be nil when nodeID == "" (the startup proto path, which
		// skips the wrap), so the closures below are always on a live sink.
		wrapped = dag.NewAdmittingLLM(wm, b.admission, admissionSpecFor(b.cfg)(b.name),
			func() { sink(stream.NodeQueued(nodeID)) }, func() { sink(stream.NodeAdmitted(nodeID)) })
	}
	// Shared with the skill toolset below so load_skill counts against this node's registry-tool budget.
	repeats := tools.NewRepeatStates()
	var builtins []tool.Tool
	if len(b.toolNames) > 0 {
		if builtins, err = tools.Build(b.toolNames, tools.Deps{
			WebSearch:          tools.Backend{Kind: b.cfg.Tools["web_search"].Kind, URL: b.cfg.Tools["web_search"].URL, Key: b.cfg.Tools["web_search"].APIKey()},
			Fetch:              tools.Backend{Kind: b.cfg.Tools["web_fetch"].Kind, URL: b.cfg.Tools["web_fetch"].URL},
			Summarizer:         wrapped,
			Cache:              b.urlCache,
			Sessions:           b.sessions,
			Workspace:          b.jail,
			WorkspaceUserID:    localUserID,
			WorkspaceCaps:      b.workspaceCaps,
			Guards:             b.cfg.Workspace.Guards,
			SafetyJudge:        b.safetyJudge,
			NodeCancelled:      b.nodeCancelled,
			RepeatGuardTripped: b.repeatGuardTripped,
			ExtTools:           b.extToolsByName,
			Memory:             b.taskStore,
			MemoryRole:         b.ac.Memory.Bucket,
			Ledger:             b.ledgerStore,
			Repeats:            repeats,
			RecordStore:        rc,
			NodeID:             nodeID,
			Coords:             coords,
			Sink:               sink,
			TurnID:             turnID,
			CallScope:          scope,
		}); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("tools: %w", err)
		}
	}
	if b.memSvc != nil {
		builtins = append(builtins, memory.NewPreload())
	}
	// extraTools: this node's artifact tools, built per dispatch; buildWorker(nil) at startup gets none.
	builtins = append(builtins, tools.SelectArtifactTools(extraTools, b.ac.Tools, b.bundle.Card.Artifact != "")...)
	var toolsets []tool.Toolset
	if len(b.ac.Skills) > 0 {
		toolsets = []tool.Toolset{tools.RepeatWrapToolset(b.agentSkillTS, repeats, b.repeatGuardTripped, scope)}
	}
	wag, err := agent.Build(b.bundle, prompts, wrapped, builtins, toolsets, b.memGuidance, b.grading, drain, meter)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build: %w", err)
	}
	return wag, wrapped, wm, builtins, nil
}

func (b *nativeNodeBuilder) build(ctx context.Context, nodeKey, advisorToken string, drain func() string, artifacts artifact.Service, appName, userID, chatID, nodeID string, sink func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, roundCoordsSetter, promptRefresher, nodeRelease, error) {
	// One holder per dispatch: two nodes of this agent run concurrently, and a
	// shared one would let either move the other's prompt mid-round.
	prompts := b.bundle.PinPrompt(b.res)
	var extraTools []tool.Tool
	var setRoundCoords func(round int, turnID, headSHA, triggerAnnotation string)
	var rc *recordstore.Client
	var coords *tools.RoundCoords
	if artifacts != nil {
		rc = recordstore.New(artifacts, appName, userID, chatID)
		// PGStore only, like executor.SetWALLedger: write_<kind> records parent_revision only over a
		// transactional ledger.
		if pg, ok := b.ledgerStore.(*ledger.PGStore); ok {
			rc = rc.WithLedger(pg)
		}
		if b.schemas != nil {
			rc = rc.WithSchemas(b.schemas)
		}
		coords = &tools.RoundCoords{}
		var terr error
		if extraTools, terr = tools.BuildNativeArtifactTools(rc, nodeID, coords, vetting.DocumentHint(chatID), vetting.SubjectHint(chatID)); terr != nil {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("artifact tools: %w", terr)
		}
		setRoundCoords = func(round int, turnID, headSHA, triggerAnnotation string) {
			*coords = tools.RoundCoords{Round: round, TurnID: turnID, HeadSHA: headSHA, TriggerAnnotation: triggerAnnotation}
		}
	}
	scope := tools.CallScope{AdvisorToken: advisorToken, ChatID: chatID, UserID: userID}
	meter := agent.NewPromptMeter()
	wag, wm, overridable, builtins, err := b.buildWorker(prompts, drain, rc, nodeID, coords, sink, stream.TurnIDFromContext(ctx), scope, meter, extraTools...)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	var mem adkmemory.Service
	if b.memSvc != nil {
		mem = b.memSvc(scope.AdvisorToken)
	}
	comp := b.compactionFor(b.ac, wm)
	comp.Meter = meter
	srv, err := agent.Serve(wag, b.sessions, mem, artifacts, comp, nodeID, sink)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("a2a serve: %w", err)
	}
	workerContextID := agent.WorkerSessionID(chatID, nodeID)
	client, err := srv.ClientForNode(nodeKey, workerContextID)
	if err != nil {
		_ = srv.Close()
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("a2a client: %w", err)
	}
	// The deterministic worker session created by this node's first dispatch outlives it
	// - reaped only at chat archive/delete (ReapNodeSessions), so a later reuse finds it.
	release := b.nodeServers.track(srv)
	refresh := b.bindPromptRefresher(prompts, overridable)
	return client, wm, builtins, setRoundCoords, refresh, release, nil
}

// bindPromptRefresher: a resolved artifact's Config can rebind the round's model/provider/effort; an
// invalid value falls back to the static binding and logs once per bad value.
func (b *nativeNodeBuilder) bindPromptRefresher(prompts *artifactsrc.Pinned, overridable *inference.OverridableModel) promptRefresher {
	// L1: overridable was built FROM this same base model - reuse it, never a second
	// NewModel call whose error a plain `_` would drop (a nil Set would panic).
	static := overridable.Get()
	var lastBad string
	var lastBadBuild string
	var cachedKey string
	var cachedModel model.LLM
	return func(ctx context.Context) artifactsrc.Artifact {
		art := prompts.Refresh(ctx)
		bound, err := b.cfg.ResolveBinding(b.prov, b.ac.Model, art.Config)
		if err != nil {
			if err.Error() != lastBad {
				lastBad = err.Error()
				slog.Warn("prompt binding invalid; using the static binding",
					"component", "artifacts", "artifact", art.Name, "err", err)
			}
			overridable.Set(static)
			return art
		}
		lastBad = ""
		if bound == nil {
			overridable.Set(static)
			return art
		}
		// This closure serves one node's sequential rounds, so a plain cache keyed on the tuple suffices.
		key := bound.ProviderName + "|" + bound.Provider.Endpoint + "|" + bound.Model + "|" + bound.Effort
		if key == cachedKey && cachedModel != nil {
			overridable.Set(cachedModel)
			return art
		}
		m, err := inference.NewModel(bound.Provider, bound.Model, b.artifacts, b.cfg.ModelCost(bound.Model), bound.Effort)
		if err != nil {
			if err.Error() != lastBadBuild {
				lastBadBuild = err.Error()
				slog.Warn("prompt binding model build failed; using the static binding",
					"component", "artifacts", "artifact", art.Name, "err", err)
			}
			overridable.Set(static)
			return art
		}
		lastBadBuild = ""
		cachedKey, cachedModel = key, m
		overridable.Set(m)
		return art
	}
}

// resolveGateCfg resolves an agent's trust-gate config, recording gated ones in gateCfgs with the
// closure that re-resolves their artifacts at run start.
func resolveGateCfg(cfg *config.Config, res *artifactsrc.Resolver, base vetting.Config, name string, ac config.AgentConfig, taskMemAvailable bool, memGuidance string, memArt artifactsrc.Artifact, bundle *agent.Bundle, gateCfgs *gateConfigs, reg pluginreg.FetchRegistry) (string, error) {
	if cfg.Gates.Enabled() {
		c, err := perAgentGateCfg(context.Background(), res, base, name, ac, taskMemAvailable, memGuidance)
		if err != nil {
			return "", err
		}
		c.MemoryArtifact = memArt
		c.Plugins = acpRegistryPluginRefs(cfg, reg)()
		c = stampBundle(c, bundle)
		gateCfgs.boot[name] = c
		gateCfgs.refresh[name] = func(ctx context.Context) (vetting.Config, error) {
			return refreshGateCfg(ctx, res, cfg, ac, c, reg)
		}
		return promptbuilder.GradingFacts(c.Threshold, c.JudgeRounds, c.ReadOnly, c.RequireRetrieval), nil
	}
	return "", nil
}

// stampBundle carries the bundle's ledger provenance onto a gate config.
func stampBundle(c vetting.Config, b *agent.Bundle) vetting.Config {
	c.BundleHash = b.Hash
	c.PromptSource = b.PromptSource
	c.PromptVersionID = b.PromptVersion
	c.PromptArtifact = b.PromptArtifact
	return c
}

// refreshGateCfg re-resolves only the artifact-backed parts (global rubric, constitution, agent rubric,
// bundle hash and provenance); config-derived fields stay as boot computed them.
func refreshGateCfg(ctx context.Context, res *artifactsrc.Resolver, cfg *config.Config, ac config.AgentConfig, boot vetting.Config, reg pluginreg.FetchRegistry) (vetting.Config, error) {
	base, err := vetting.FromConfig(ctx, res, cfg.Gates)
	if err != nil {
		return boot, err
	}
	c := boot
	c.Constitution, c.ConstitutionArtifact = base.Constitution, base.ConstitutionArtifact
	c.Rubric, c.RubricArtifact, c.RubricSpecs, c.RubricFixes = base.Rubric, base.RubricArtifact, base.RubricSpecs, base.RubricFixes
	if override, specs, fixes, art, err := vetting.LoadBundleRubricSpecs(ctx, res, ac.Bundle); err != nil {
		return boot, err
	} else if override != "" {
		c.Rubric, c.RubricSpecs, c.RubricFixes, c.RubricArtifact = override, specs, fixes, art
	}
	c.Plugins = acpRegistryPluginRefs(cfg, reg)()
	b, err := agent.LoadBundle(ctx, res, ac.Bundle)
	if err != nil {
		return boot, err
	}
	return stampBundle(c, b), nil
}

// buildNativeNode builds one native (co-located) configured agent: bundle, memory view, scoped
// skills, gate grading, and the per-dispatch worker builder.
func buildNativeNode(name string, ac config.AgentConfig, prov config.ProviderConfig, taskStore *memory.Store, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), builtinSkillSrc skill.Source, cfg *config.Config, res *artifactsrc.Resolver, workspaceCaps workspace.Caps, jail *workspace.Jail, safetyJudge tools.SafetyJudge, nodeCancelled func(chatID, nodeID string) bool, repeatGuardTripped func(chatID, nodeID, msg string) bool, extToolsByName map[string]tool.Tool, urlCache *tools.URLCache, sessions session.Service, artifacts artifact.Service, ledgerStore ledger.LedgerStore, compactionFor func(ac config.AgentConfig, workerModel model.LLM) agent.Compaction, gateCfg vetting.Config, gateCfgs *gateConfigs, nodeServers *perNodeServers, reg pluginreg.FetchRegistry, admission *dag.Admission) (adkagent.Agent, error) {
	toolNames := resolveToolNames(ac.Tools, taskStore != nil)

	bundle, err := agent.LoadBundle(context.Background(), res, ac.Bundle)
	if err != nil {
		return nil, fmtErr(name, "bundle: %v", err)
	}
	var memGuidance string
	var memArt artifactsrc.Artifact
	if taskStore != nil {
		if memGuidance, memArt, err = agent.LoadBundleMemory(context.Background(), res, ac.Bundle); err != nil {
			return nil, fmtErr(name, "memory.md: %v", err)
		}
	}
	var memSvc func(token string) adkmemory.Service
	if taskStore != nil && memGuidance != "" {
		memSvc = func(token string) adkmemory.Service {
			return taskStore.View(memory.Scope{Role: ac.Memory.Bucket, Legacy: name}, nodeMemoryScope(jail, token))
		}
	}
	agentSkillTS, err := newScopedSkillTS(ac.Skills)
	if err != nil {
		return nil, fmtErr(name, "skills toolset: %v", err)
	}
	// Fails boot on a malformed scoped skill source rather than at the node's first load_skill.
	if _, err := skillsource.Scoped(builtinSkillSrc, ac.Skills).ListFrontmatters(context.Background()); err != nil {
		return nil, fmtErr(name, "skills: %v", err)
	}

	grading, err := resolveGateCfg(cfg, res, gateCfg, name, ac, taskStore != nil, memGuidance, memArt, bundle, gateCfgs, reg)
	if err != nil {
		return nil, fmtErr(name, "rubric: %v", err)
	}

	b := &nativeNodeBuilder{
		name:               name,
		prov:               prov,
		ac:                 ac,
		artifacts:          artifacts,
		cfg:                cfg,
		admission:          admission,
		toolNames:          toolNames,
		urlCache:           urlCache,
		sessions:           sessions,
		jail:               jail,
		safetyJudge:        safetyJudge,
		nodeCancelled:      nodeCancelled,
		repeatGuardTripped: repeatGuardTripped,
		extToolsByName:     extToolsByName,
		taskStore:          taskStore,
		memSvc:             memSvc,
		workspaceCaps:      workspaceCaps,
		memGuidance:        memGuidance,
		bundle:             bundle,
		agentSkillTS:       agentSkillTS,
		grading:            grading,
		ledgerStore:        ledgerStore,
		compactionFor:      compactionFor,
		nodeServers:        nodeServers,
		res:                res,
		schemas:            gateCfg.Schemas,
	}
	protoAgent, _, _, _, err := b.buildWorker(bundle.PinPrompt(res), nil, nil, "", nil, nil, "", tools.CallScope{}, nil)
	if err != nil {
		return nil, fmtErr(name, "%w", err)
	}
	na := nativeAgent{
		Agent: protoAgent,
		build: b.build,
	}
	agentTools := ac.Tools
	if artifacts != nil {
		// Artifact tools are built per dispatch; listed here too so debugging sees what was offered.
		agentTools = append(append([]string{}, ac.Tools...), "list_artifacts", "read_artifact")
		if bundle.Card.Artifact != "" {
			agentTools = append(agentTools, "write_artifact", "edit_artifact")
		}
	}
	slog.Info("agent serving over A2A per DAG node", "component", "startup", "agent", name, "tools", agentTools)
	return na, nil
}

func bootReconcile(reconcile bool, cfg *config.Config, st *store.Store, jail *workspace.Jail) ([]store.ResumableNode, error) {
	var resumeNodes []store.ResumableNode
	if reconcile {
		id, err := store.LoadOrCreateInstanceID(cfg.Workspace.Root)
		if err != nil {
			return nil, fmt.Errorf("instance id init failed: %w", err)
		}
		st.SetInstanceID(id)
		// Runs before anything can register a run with the Hub, so resume starts from a settled DB; the memory
		// consolidator's boot sweep starts after.
		resumeNodes = reconcileNodes(context.Background(), st, jail, func(chatID, pauseReason string) (bool, string) {
			// Never resume an archived chat's paused nodes; the archive should free their run slots.
			c, _ := st.GetChat(context.Background(), chatID)
			p, _ := st.GetLatestDagPlan(context.Background(), chatID)
			var planCreatedAt time.Time
			if p != nil {
				planCreatedAt = p.CreatedAt
			}
			if ok, why := resumeGuardArchivedOrStale(c != nil && c.Archived, p != nil, dag.PauseReason(pauseReason), planCreatedAt); !ok {
				return false, why
			}
			// A resumable node was provisioned a chat scope dir; if the
			// workspace is gone the run cannot pick up where it left off.
			if _, rerr := jail.Resolve(st.SessionUserForChat(context.Background(), chatID), chatID, "."); rerr != nil {
				return false, "workspace dir is gone"
			}
			return true, ""
		})
	}
	return resumeNodes, nil
}

func openMemoryStores(ctx context.Context, cfg *config.Config, st *store.Store, artifacts artifact.Service, admission *dag.Admission) (*memory.Store, *memory.Store, []func(), error) {
	var taskStore, userStore *memory.Store
	var startSweeps []func()
	openMemory := func(rm config.ResolvedMemory, domain string) (*memory.Store, error) {
		eprov, ok := cfg.Provider(rm.Embedder.Provider)
		if !ok {
			return nil, fmt.Errorf("embedder provider %q not found", rm.Embedder.Provider)
		}
		embedder, err := inference.NewEmbedder(eprov, rm.Embedder.Model, cfg.ModelCost(rm.Embedder.Model))
		if err != nil {
			return nil, fmt.Errorf("embedder: %w", err)
		}
		// recall runs in the node's own ctx (real per-round coords); commit fires from
		// ctx-less background/tool calls, so "embed" is its fallback name (not the consolidator's "memory").
		setDefaultAgent(embedder, "embed")
		cprov, ok := cfg.Provider(rm.Consolidation.Provider)
		if !ok {
			return nil, fmt.Errorf("consolidation provider %q not found", rm.Consolidation.Provider)
		}
		consolidator, err := inference.NewModel(cprov, rm.Consolidation.Model, artifacts, cfg.ModelCost(rm.Consolidation.Model), cfg.ModelEffort(rm.Consolidation.Model))
		if err != nil {
			return nil, fmt.Errorf("consolidation model: %w", err)
		}
		// Commit runs from a background goroutine (user memory hook) or a tool call
		// whose ctx lost its node coords - never a DAG node's own model call.
		setDefaultAgent(consolidator, "memory")
		consolidator = dag.NewAdmittingLLM(consolidator, admission, lightweightSpec(cfg, rm.Consolidation.Model), nil, nil)
		s, err := memory.New(context.Background(), rm.Kind, rm.URL, embedder, consolidator, rm.Collection, domain, rm.TopK, rm.MinScore)
		if err != nil {
			return nil, err
		}
		// internal/memory can't import internal/store, so the memory_ops audit sink is wired here.
		s.SetOpsLog(storeOpsLog{st})
		return s, nil
	}
	// Started after resumed nodes are dispatched so a boot resume never contends with the sweep for a chat.
	if rm, ok := cfg.MemoryStore("stage_memory"); ok {
		s, err := openMemory(rm, "task")
		if err != nil {
			return nil, nil, nil, fmt.Errorf("task memory init failed: %w", err)
		}
		taskStore = s
		slog.Info("semantic memory enabled", "component", "startup", "collection", rm.Collection,
			"embedder", rm.Embedder.Model, "consolidation", rm.Consolidation.Model)
		startSweeps = append(startSweeps, func() { startConsolidationSweep(ctx, s, rm, cfg.Location()) })
	}
	if slices.Contains(cfg.Orchestrator.Tools, "commit_memory") {
		if rm, ok := cfg.MemoryStore("commit_memory"); ok {
			s, err := openMemory(rm, "user")
			if err != nil {
				return nil, nil, nil, fmt.Errorf("user memory init failed: %w", err)
			}
			userStore = s
			slog.Info("user memory enabled", "component", "startup", "collection", rm.Collection)
			startSweeps = append(startSweeps, func() { startConsolidationSweep(ctx, s, rm, cfg.Location()) })
		}
	}

	return taskStore, userStore, startSweeps, nil
}

func discoverSDKToolSources(sdkExts []builtSDKExtension) (tools.GitTokenSource, vetting.DeliverFunc, tools.AssignmentFreshnessFunc, tools.AssignmentMetaFunc) {
	gitCredSrc, gitCredSrcName := findGitCredentialSource(sdkExts)
	deliverer, delivererName := findDeliverer(sdkExts)
	var gitTokenSource tools.GitTokenSource
	if gitCredSrc != nil {
		gitTokenSource = sdkGitCredentialAdapter{src: gitCredSrc}
		slog.Info("extension supplies git credentials", "component", "startup", "extension", gitCredSrcName)
	}
	var deliver vetting.DeliverFunc
	if deliverer != nil {
		deliver = sdkDeliverAdapter{deliverer: deliverer}.Deliver
		slog.Info("extension supplies delivery", "component", "startup", "extension", delivererName)
	}
	freshnessChecker, freshnessCheckerName := findAssignmentFreshnessChecker(sdkExts)
	var assignmentFreshness tools.AssignmentFreshnessFunc
	if freshnessChecker != nil {
		assignmentFreshness = func(ctx adkagent.Context, planID, agentName, contextID string, a dag.Assignment) (bool, string) {
			return freshnessChecker.BeforeAssignment(ctx, toSDKAssignment(planID, agentName, contextID, a))
		}
		slog.Info("extension supplies assignment freshness checks", "component", "startup", "extension", freshnessCheckerName)
	}
	metaExtension, metaExtensionName := findAssignmentMetaExtension(sdkExts)
	var assignmentMeta tools.AssignmentMetaFunc
	if metaExtension != nil {
		assignmentMeta = func(ctx adkagent.Context, planID, agentName string, a dag.Assignment) (string, map[string]any) {
			return metaExtensionName, metaExtension.OnAssignment(ctx, toSDKAssignment(planID, agentName, "", a))
		}
		slog.Info("extension supplies assignment meta", "component", "startup", "extension", metaExtensionName)
	}
	return gitTokenSource, deliver, assignmentFreshness, assignmentMeta
}

func buildAgentInfos(ctx context.Context, cfg *config.Config, res *artifactsrc.Resolver, clientMap map[string]adkagent.Agent) ([]dag.AgentInfo, map[string]bool, string, map[string]string) {
	agentInfos := make([]dag.AgentInfo, 0, len(clientMap))
	mediaAgents := make(map[string]bool)
	bundleHashes := make(map[string]string, len(clientMap))
	for name, c := range clientMap {
		ac := cfg.Agents[name]
		// Re-reads a bundle buildAgents already loaded, rather than widen its
		// already-long return signature for this one optional field.
		var defaultArtifact string
		if bundle, err := agent.LoadBundle(ctx, res, ac.Bundle); err != nil {
			slog.Warn("agent bundle: re-read for default artifact failed", "component", "startup", "agent", name, "err", err)
		} else {
			defaultArtifact, bundleHashes[name] = bundle.Card.Artifact, bundle.Hash
		}
		agentInfos = append(agentInfos, dag.AgentInfo{Name: name, Description: c.Description(), ContextWindow: ac.ContextWindow, DefaultArtifact: defaultArtifact})
		for _, inp := range ac.Inputs {
			if inp == "image" || inp == "audio" {
				mediaAgents[name] = true
				break
			}
		}
	}
	sort.Slice(agentInfos, func(i, j int) bool { return agentInfos[i].Name < agentInfos[j].Name })
	var rosterSB strings.Builder
	for _, a := range agentInfos {
		fmt.Fprintf(&rosterSB, "- `%s` - %s\n", a.Name, a.Description)
	}
	return agentInfos, mediaAgents, rosterSB.String(), bundleHashes
}

func assembleOrchestrator(ctx context.Context, cfg *config.Config, res *artifactsrc.Resolver, st *store.Store, llm model.LLM, roster *dag.Roster, judgeFactory vetting.JudgeFactory, planJudge vetting.PlanJudge, decisions *decide.Decider, taskStore, userStore *memory.Store, artifacts artifact.Service, ledgerStore ledger.LedgerStore, assignmentFreshness tools.AssignmentFreshnessFunc, assignmentMeta tools.AssignmentMetaFunc, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), skillSrc skill.Source, orchRef *atomic.Pointer[orchestrator.Orchestrator], executorRef *atomic.Pointer[dag.Executor], hooks *shutdownHooks, setupFn dag.SetupFunc, admission *dag.Admission, artifactSchemas *artifactschema.Registry) (*orchestrator.Orchestrator, error) {
	orchBundle, err := agent.LoadBundle(ctx, res, "agents/orchestrator")
	if err != nil {
		return nil, fmt.Errorf("orchestrator bundle load failed: %w", err)
	}
	// Bare names: on-disk dotagents shadows the embedded quack:format-markdown, so resolve bare -> qualified.
	fmFm, err := skillsource.Resolve(context.Background(), skillSrc, "format-markdown")
	if err != nil {
		return nil, fmt.Errorf("format-markdown skill load failed: %w", err)
	}
	planWorkFm, err := skillsource.Resolve(context.Background(), skillSrc, "plan-work")
	if err != nil {
		return nil, fmt.Errorf("plan-work skill load failed: %w", err)
	}
	orchBehaviour := orchBundle.Prompt
	if userStore != nil {
		mem, _, err := agent.LoadBundleMemory(ctx, res, "agents/orchestrator")
		if err != nil {
			return nil, fmt.Errorf("orchestrator memory.md load failed: %w", err)
		}
		if mem != "" {
			orchBehaviour += "\n\n" + mem
		}
	}
	orchSkillTS, err := newScopedSkillTS(cfg.Orchestrator.Skills)
	if err != nil {
		return nil, fmt.Errorf("orchestrator skills toolset init failed: %w", err)
	}

	planner := dag.NewPlanner(roster.Infos, cfg.Workspace.CheckCommands, planJudge)
	planner.SetDecisions(decisions)
	executor := dag.NewExecutor(st.Sessions, nil, nil, judgeFactory, nil, nil)
	executor.SetRoster(roster)
	orchSysPrompt := promptbuilder.CacheByDay(func(ctx context.Context) string {
		return strconv.FormatUint(executor.RosterFor(ctx).Gen, 10)
	}, func(ctx context.Context) string {
		return promptbuilder.Orchestrator(executor.RosterFor(ctx).Text, []*skill.Frontmatter{fmFm, planWorkFm}, orchBehaviour)
	})
	executor.SetMaxActive(cfg.Dag.MaxActiveNodes)
	executor.SetAdmission(admission, judgeSpec(cfg))
	executor.SetSetup(setupFn)
	executor.SetArtifacts(artifacts)
	if ledgerStore != nil {
		executor.SetWALLedger(ledgerStore)
	}
	executor.SetSchemas(artifactSchemas)
	executor.SetDecisions(decisions)
	executor.SetNodeStateStore(st) // write-through node state machine
	executorRef.Store(executor)
	// Orchestrator turns take a session from the worker nodes' pool, held only while
	// generating (holding across the DAG would deadlock them); wraps AFTER setDefaultAgent.
	orchLLM := dag.NewAdmittingLLM(llm, admission, orchestratorSpec(cfg), nil, nil)
	// Hard backstop under ADK's own compaction - see BudgetedLLM's doc for why.
	orchLLM = dag.NewBudgetedLLM(orchLLM, cfg.Orchestrator.ContextWindow)
	orch := orchestrator.New(st.Sessions, orchLLM, orchSysPrompt, planner, executor, orchSkillTS, userStore, taskStore)
	// Unconditional: dag_plan persistence must not depend on load_artifacts being in orchestrator.tools.
	orch.SetArtifacts(artifacts)
	orch.SetDecisions(decisions)
	orch.SetNodeSessionReaper(st.ReapNodeSessions)
	orch.SetPlanLoader(st.LoadExecPlan)
	orch.SetAssignmentFreshnessCheck(assignmentFreshness)
	orch.SetAssignmentMetaHook(assignmentMeta)
	// Same source of truth as buildAgents' per-node compactionFor (cfg.Session.Compaction),
	// built once for the orchestrator's own long-lived chat session, which it never sees (#A3).
	if orchCompCfg := cfg.Session.Compaction; orchCompCfg.Enabled {
		if cfg.Orchestrator.ContextWindow <= 0 {
			slog.Warn("context compaction enabled but orchestrator.context_window is unset; not compacting the chat session", "component", "startup")
		} else {
			// kv 0, not orchestratorSpec: a compaction pass is a one-shot summarise
			// call, not the turn whose window that spec sizes.
			orchComp, cerr := agent.NativeCompactionConfig(agent.Compaction{
				Summarizer:         dag.NewAdmittingLLM(llm, admission, lightweightSpec(cfg, cfg.Orchestrator.Model), nil, nil),
				ContextWindow:      cfg.Orchestrator.ContextWindow,
				Enabled:            true,
				TokenThreshold:     orchCompCfg.TokenThreshold,
				EventRetentionSize: orchCompCfg.EventRetentionSize,
				CompactionInterval: orchCompCfg.CompactionInterval,
				OverlapSize:        orchCompCfg.OverlapSize,
			})
			if cerr != nil {
				return nil, fmt.Errorf("compaction: orchestrator: %w", cerr)
			}
			orch.SetCompaction(orchComp)
		}
	}
	if ledgerStore != nil {
		orch.SetLedger(ledgerStore)
	}
	if artifactSchemas != nil {
		orch.SetSchemas(artifactSchemas)
	}
	orch.SetRenderUI(slices.Contains(cfg.Orchestrator.Tools, "render_ui"))
	orchRef.Store(orch)
	if hooks != nil {
		hooks.pauser = executor
	}
	return orch, nil
}

func mountHTTP(cfg *config.Config, st *store.Store, orch *orchestrator.Orchestrator, llm model.LLM, jail *workspace.Jail, runHub *stream.Hub, ledgerStore ledger.LedgerStore, taskStore, userStore *memory.Store, artifacts *store.TurnAwareService, sdkExts []builtSDKExtension, authMW *auth.Auth, reload *reloader, pluginReg pluginreg.FetchRegistry) (http.Handler, error) {
	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, fmt.Errorf("embed SPA fs failed: %w", err)
	}

	restHandler := rest.NewHandler(st, orch, llm, jail, runHub, ledgerStore, Version, taskStore, userStore, artifacts, extensionDescriptors(sdkExts))
	restPlugins := rest.NewPlugins(pluginReg, cfg.Plugins.Root, cfg.Plugins.Seed, reload.reload, reload.mcpDeclared)
	restHandler.SetPlugins(restPlugins)
	restHandler.SetTraceURLTemplate(cfg.Observability.Otel.TraceURLTemplate)

	return server.New(server.Options{
		REST:          restHandler,
		MCP:           mcpserver.Handler(orch),
		SPA:           spa,
		SDKExtensions: sdkExtensionMounts(sdkExts),
		Auth:          authMW,
	}), nil
}

func startWorkspaceGC(ctx context.Context, cfg *config.Config, jail *workspace.Jail, runHub *stream.Hub) error {
	gcHomeDir, err := jail.HomeDir(localUserID)
	if err != nil {
		return fmt.Errorf("workspace gc home dir init failed: %w", err)
	}
	gcCaps := workspace.Caps{
		Timeout:   time.Duration(cfg.Workspace.TimeoutSeconds) * time.Second,
		ExtraPath: cfg.Workspace.ExecPath,
		Env:       cfg.Workspace.Env,
		HomeDir:   gcHomeDir,
	}
	gcCfg := workspace.GCConfig{
		Enabled:      cfg.Workspace.GC.IsEnabled(),
		ChatTTL:      time.Duration(cfg.Workspace.GC.ChatTTLHours) * time.Hour,
		ScratchTTL:   time.Duration(cfg.Workspace.GC.ScratchTTLHours) * time.Hour,
		HomeMaxBytes: int64(cfg.Workspace.GC.HomeMaxMB) * 1024 * 1024,
		Interval:     time.Duration(cfg.Workspace.GC.IntervalHours) * time.Hour,
	}
	// GC sees on-disk dir names; match them against active chat ids raw or via ChatDirName.
	gcActive := func(chatDir string) bool {
		for _, id := range runHub.ActiveChatIDs() {
			if id == chatDir || workspace.ChatDirName(id) == chatDir {
				return true
			}
		}
		return false
	}
	go workspace.RunGC(ctx, jail, gcCfg, gcActive, func(pctx context.Context, root, dir string) error {
		return tools.PruneWorktree(pctx, root, dir, gcCaps)
	})
	return nil
}

// nodeMemoryScope is the memory scope of the node registered under its build-time token, never
// a prompt marker; a miss adds nothing, leaving the agent's base buckets (never another node's).
func nodeMemoryScope(jail *workspace.Jail, token string) func(context.Context) memory.Scope {
	return func(context.Context) memory.Scope {
		at, ok := vetting.LookupAdvisorThread(token)
		if !ok {
			return memory.Scope{}
		}
		sc := memory.Scope{User: at.UserID}
		if jail != nil {
			sc.Repo = jail.RepoKey(localUserID, at.ChatID)
		}
		return sc
	}
}

// setupCloneFunc returns the dag.SetupFn that clones a chat repo for a new node.
func setupCloneFunc(cfg *config.Config, jail *workspace.Jail, workspaceCaps workspace.Caps, gitCredentials []tools.GitCredential, gitTokenSource tools.GitTokenSource) dag.SetupFunc {
	return func(ctx context.Context, _, chatID, dir string, setup dag.Setup) error {
		_, err := tools.SetupClone(ctx, jail, localUserID, chatID, dir, setup.Repo, setup.BaseRef, setup.WorkBranch, setup.CheckoutExistingHead, workspaceCaps, gitCredentials, gitTokenSource, cfg.Workspace.CheckSetup)
		return err
	}
}

// buildCompaction builds the per-node compaction resolver (and its optional fallback
// summarizer) from the session.compaction config.
func buildCompaction(cfg *config.Config, res *artifactsrc.Resolver, artifacts artifact.Service) (func(ac config.AgentConfig, workerModel model.LLM) agent.Compaction, error) {
	var fallbackSummarizer model.LLM
	compCfg := cfg.Session.Compaction
	if compCfg.Enabled && compCfg.Model != "" {
		cprov, ok := cfg.Provider(compCfg.Provider)
		if !ok {
			return nil, fmt.Errorf("compaction: provider %q not found", compCfg.Provider)
		}
		var err error
		if fallbackSummarizer, err = inference.NewModel(cprov, compCfg.Model, artifacts, cfg.ModelCost(compCfg.Model), cfg.ModelEffort(compCfg.Model)); err != nil {
			return nil, fmt.Errorf("compaction: model: %w", err)
		}
		// Fallback only (ResolveSummarizer prefers the worker model); unwrapped -
		// ADK compacts mid-turn, inside the node's own held reservation.
		setDefaultAgent(fallbackSummarizer, "compaction")
		slog.Info("context compaction enabled", "component", "startup", "fallback_summariser", compCfg.Model)
	} else if compCfg.Enabled {
		slog.Info("context compaction enabled", "component", "startup", "summariser", "active worker model (no fallback configured)")
	}
	return func(ac config.AgentConfig, workerModel model.LLM) agent.Compaction {
		if !compCfg.Enabled {
			return agent.Compaction{}
		}
		if ac.ContextWindow == 0 {
			slog.Warn("context compaction: agent has no context_window configured; not compacting it", "component", "startup", "model", ac.Model)
			return agent.Compaction{}
		}
		return agent.Compaction{
			Prompts:            res,
			Summarizer:         agent.ResolveSummarizer(workerModel, fallbackSummarizer),
			ContextWindow:      ac.ContextWindow,
			Enabled:            true,
			TokenThreshold:     compCfg.TokenThreshold,
			EventRetentionSize: compCfg.EventRetentionSize,
			CompactionInterval: compCfg.CompactionInterval,
			OverlapSize:        compCfg.OverlapSize,
		}
	}, nil
}

// perAgentGateCfg specializes the base trust-gate config for one agent.
func perAgentGateCfg(ctx context.Context, res *artifactsrc.Resolver, base vetting.Config, name string, ac config.AgentConfig, memEnabled bool, memGuidance string) (vetting.Config, error) {
	c := base
	c.CommitMemory = memEnabled && memGuidance != ""
	if memEnabled && ac.Memory.Bucket != "" && memGuidance == "" {
		slog.Warn("agent has a memory bucket but no memory.md; it will recall but never commit",
			"component", "serve", "agent", name, "bucket", ac.Memory.Bucket, "bundle", ac.Bundle)
	}
	c.MemoryRole = ac.Memory.Bucket
	c.ReadOnly, c.RequireRetrieval = vetting.AgentToolPolicy(ac.Tools, ac.Acp)
	if ac.Acp != nil {
		c.ExternalWorker = true
	}
	if override, specs, fixes, art, err := vetting.LoadBundleRubricSpecs(ctx, res, ac.Bundle); err != nil {
		return c, err
	} else if override != "" {
		c.Rubric = override
		c.RubricSpecs = specs
		c.RubricFixes = fixes
		c.RubricArtifact = art
		slog.Info("using per-agent rubric from bundle", "component", "startup", "agent", name)
	}
	return applyAgentJudgeOverrides(c, ac, name), nil
}

// acpPermJudge wraps the safety judge as an ACP permission gate; an
// unavailable judge fails open (allowing) and logs it.
func acpPermJudge(sj tools.SafetyJudge, agentName string) func(ctx context.Context, toolName, title string, input map[string]any) (bool, string) {
	return func(ctx context.Context, toolName, title string, input map[string]any) (bool, string) {
		otelobs.RecordPermissionAsk(agentName)
		allow, reason, err := sj(ctx,
			fmt.Sprintf("the external %s agent asks permission for: %s", agentName, title),
			"", toolName, input, "")
		if err != nil {
			slog.Warn("acp permission judge unavailable; allowing", "component", "acp", "agent", agentName, "err", err)
			return true, "judge unavailable"
		}
		return allow, reason
	}
}

// applyAgentJudgeOverrides applies the per-agent judge-rounds overrides and logs the result.
func applyAgentJudgeOverrides(c vetting.Config, ac config.AgentConfig, name string) vetting.Config {
	if ac.JudgeRounds > 0 {
		c.JudgeRounds = ac.JudgeRounds
	}
	if ac.Judge != nil && !*ac.Judge {
		c.JudgeRounds = 0
	}
	slog.Info("per-agent trust gate config", "component", "startup", "agent", name, "judge_rounds", c.JudgeRounds)
	return c
}

// acpChildEnv merges workspace.env (deployment-wide) with acp.env (agent-specific, wins on shared key).
func acpChildEnv(workspaceEnv, agentEnv map[string]string) []string {
	merged := make(map[string]string, len(workspaceEnv)+len(agentEnv))
	maps.Copy(merged, workspaceEnv)
	maps.Copy(merged, agentEnv)
	env := make([]string, 0, len(merged))
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		env = append(env, k+"="+merged[k])
	}
	return env
}

// piACPEnv generates PI_ACP_CONFIG: only the fields the pi-acp shim reads (tools/pi-acp/pi-acp.mjs).
// allow_clone lifts checkPolicy's clone deny only where the sandbox OS-enforces the read-only work tree.
func piACPEnv(prov config.ProviderConfig, ac config.AgentConfig, skillPaths []string, sandbox workspace.SandboxMode) []string {
	type m = map[string]any
	apiKey := prov.APIKey
	if apiKey == "" {
		apiKey = "unused"
	}
	cfg := m{
		"endpoint": prov.Endpoint,
		"api_key":  apiKey,
		"model":    ac.Model,
	}
	if ac.ContextWindow > 0 {
		cfg["context_window"] = ac.ContextWindow
		// pi's own default (16384) caps reasoning+answer on the same request.
		cfg["max_output_tokens"] = 32768
	}
	if len(skillPaths) > 0 {
		cfg["skill_paths"] = skillPaths
	}
	if ac.Acp != nil && ac.Acp.AllowClone && ac.Acp.ReadOnly {
		if workspace.EnforcesBoundary(sandbox) {
			cfg["allow_clone"] = true
		} else {
			slog.Warn("acp.allow_clone ignored: sandbox does not enforce the read-only work tree", "component", "acp", "sandbox", sandbox)
		}
	}
	content, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	return []string{"PI_ACP_CONFIG=" + string(content)}
}

// extractedDotagentsSkillsDir: embedded skills on disk for the sandboxed ACP child, which reads
// skill_paths by directory and has no embedded-FS access.
var extractedDotagentsSkillsDir = filepath.Join(os.TempDir(), "quack-acp-dotagents-skills")

var extractDotagentsSkillsMu sync.Mutex

// embeddedExtractSources: the embedded quack plugin's two trees, in the same order newSkillSource merges them.
func embeddedExtractSources() []fs.FS {
	return []fs.FS{bundledir.SubFS("skills"), bundledir.SubFS(dotagentsEmbeddedSkills)}
}

// ensureExtractedDotagentsSkillNames extracts the named (bare) embedded skills idempotently; a failed
// one is simply absent.
func ensureExtractedDotagentsSkillNames(missing []string) {
	extractDotagentsSkillsMu.Lock()
	defer extractDotagentsSkillsMu.Unlock()

	want := map[string]bool{}
	for _, n := range missing {
		want[n] = true
	}
	if err := os.MkdirAll(extractedDotagentsSkillsDir, 0o755); err != nil {
		slog.Warn("acp skill extraction: could not create dir; ACP agents may miss embedded skills",
			"component", "serve", "dir", extractedDotagentsSkillsDir, "err", err)
		return
	}
	// Prune extracted skills no longer missing so they can't sit beside an on-disk copy.
	if entries, err := os.ReadDir(extractedDotagentsSkillsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !want[e.Name()] {
				_ = os.RemoveAll(filepath.Join(extractedDotagentsSkillsDir, e.Name()))
			}
		}
	}
	sources := embeddedExtractSources()
	for name := range want {
		dest := filepath.Join(extractedDotagentsSkillsDir, name)
		if _, err := os.Stat(dest); err == nil {
			continue // already extracted
		}
		if !extractEmbeddedSkill(sources, name, dest) {
			slog.Warn("acp skill extraction: skill not found in embedded FS", "component", "serve", "skill", name)
		}
	}
}

// extractEmbeddedSkill copies name's SKILL.md tree from the first of
// sources that has it. ok=false only when no source has the skill at all.
func extractEmbeddedSkill(sources []fs.FS, name, dest string) bool {
	for _, src := range sources {
		sub, err := fs.Sub(src, name)
		if err != nil {
			continue
		}
		if _, err := fs.Stat(sub, "SKILL.md"); err != nil {
			continue
		}
		if err := os.CopyFS(dest, sub); err != nil {
			slog.Warn("acp skill extraction failed for one skill; ACP agents may miss it",
				"component", "serve", "skill", name, "err", err)
			_ = os.RemoveAll(dest)
		}
		return true
	}
	return false
}

// acpSkillPaths: the local skills/ dir (skipped if a row is named "quack"), each plugin's skills/, then
// the extracted embedded backfill for whatever neither shadows.
func acpSkillPaths(plugins []plugin.Plugin) []string {
	var out []string
	localSkillsDirAdded := false
	if !hasQuackRow(plugins) {
		if abs, err := filepath.Abs("skills"); err == nil {
			if st, err := os.Stat(abs); err == nil && st.IsDir() {
				out = append(out, abs)
				localSkillsDirAdded = true
			}
		}
	}
	out = append(out, plugin.SkillDirs(plugins)...)

	// The raw local skills/ dir just added already covers quack's own
	// half on disk - only the embedded-dotagents half can still be missing.
	haveQualified, haveBare := resolvedHaveSets(plugins)
	var missing []string
	if !localSkillsDirAdded {
		missing = missingQuackOwnSkillNames(haveQualified)
	}
	missing = append(missing, missingDotagentsEmbeddedSkillNames(haveBare)...)
	if len(missing) == 0 {
		return out
	}
	bare := make([]string, len(missing))
	for i, m := range missing {
		bare[i] = skillsource.BareName(m)
	}
	ensureExtractedDotagentsSkillNames(bare)
	if st, err := os.Stat(extractedDotagentsSkillsDir); err == nil && st.IsDir() {
		out = append(out, extractedDotagentsSkillsDir)
	}
	return out
}

// hasQuackRow reports whether a plugin is registered under the exact name "quack".
func hasQuackRow(plugins []plugin.Plugin) bool {
	for _, p := range plugins {
		if p.Name == "quack" {
			return true
		}
	}
	return false
}

// registrySignature caches on names+shas. A local row's sha is "", so it invalidates only on a row-set
// change; its content is still read live from disk.
func registrySignature(rows []pluginreg.Plugin) string {
	parts := make([]string, len(rows))
	for i, p := range rows {
		parts[i] = p.Name + "@" + p.SHA
	}
	return strings.Join(parts, ",")
}

// acpRegistrySkillPaths is acp.Options.SkillPaths over a fresh registry read, cached by
// registrySignature. plugins.root itself goes through acpRegistryExtraRO.
func acpRegistrySkillPaths(cfg *config.Config, reg pluginreg.FetchRegistry) func() []string {
	var mu sync.Mutex
	var cachedKey string
	var cachedPaths []string
	return func() []string {
		rows, err := reg.List(context.Background())
		if err != nil {
			slog.Warn("acp: plugin registry list failed; skill paths may be stale", "component", "acp", "err", err)
			rows = nil
		}
		rows = pluginreg.OrderBySeed(cfg.Plugins.Seed, rows)
		key := registrySignature(rows)

		mu.Lock()
		defer mu.Unlock()
		if key == cachedKey && cachedPaths != nil {
			return cachedPaths
		}
		plugins, err := resolveRegistryPlugins(cfg.Plugins.Root, rows)
		if err != nil {
			slog.Warn("acp: plugin resolve failed; skill paths may be stale", "component", "acp", "err", err)
			plugins = nil
		}
		paths := acpSkillPaths(plugins)
		cachedKey, cachedPaths = key, paths
		return paths
	}
}

// acpRegistryExtraRO grants plugins.root to the sandbox (the pi shim reads it) without adding it to
// skill_paths.
func acpRegistryExtraRO(cfg *config.Config) func() []string {
	return func() []string {
		if st, err := os.Stat(cfg.Plugins.Root); err == nil && st.IsDir() {
			return []string{cfg.Plugins.Root}
		}
		return nil
	}
}

// acpRegistryPluginRefs: ledger provenance is every admitted row (refusals keep an Error-tagged row)
// plus the always-in-scope embedded quack bundle.
func acpRegistryPluginRefs(cfg *config.Config, reg pluginreg.FetchRegistry) func() []ledger.PluginRef {
	return func() []ledger.PluginRef {
		rows, err := reg.List(context.Background())
		if err != nil {
			rows = nil
		}
		have := make(map[string]bool, len(rows))
		var refs []ledger.PluginRef
		for _, p := range rows {
			if p.Error != "" {
				continue
			}
			refs = append(refs, ledger.PluginRef{Name: p.Name, SHA: p.SHA})
			have[p.Name] = true
		}
		if !have["quack"] {
			refs = append(refs, ledger.PluginRef{Name: "quack"})
		}
		return refs
	}
}

// acpSkillFrontmatters scopes an ACP agent's roster to its declared skills;
// an empty list falls back to the full library (no config declares one yet).
func acpSkillFrontmatters(ctx context.Context, src skill.Source, names []string) ([]*skill.Frontmatter, error) {
	if len(names) == 0 {
		return src.ListFrontmatters(ctx)
	}
	return skillsource.Scoped(src, names).ListFrontmatters(ctx)
}

// dropOptionalAgent drops an optional agent that fails to build, recording it in dropped when set:
// a plugin-seeded one on any error, a config-authored one only when its tools are unknown.
func dropOptionalAgent(name string, ac config.AgentConfig, err error, dropped map[string]error) bool {
	if !ac.Optional || (ac.SeededBy == "" && !errors.Is(err, tools.ErrUnknownTool)) {
		return false
	}
	slog.Warn("optional agent unavailable; dropped from the roster", "component", "startup", "agent", name, "err", err)
	if dropped != nil {
		dropped[name] = err
	}
	return true
}

func fmtErr(agentName, format string, args ...any) error {
	return fmt.Errorf("agent %q: "+format, append([]any{agentName}, args...)...)
}

// resolveToolNames drops builtins whose dependency is off and collapses recall_memory/load_memory (one
// tool, two names) to whichever is listed first. Artifact tools are per-dispatch.
func resolveToolNames(configured []string, taskMemAvailable bool) (names []string) {
	names = make([]string, 0, len(configured))
	sawMemoryRecall := false
	for _, t := range configured {
		if tools.IsNativeArtifactTool(t) {
			continue
		}
		switch t {
		case "stage_memory":
			if !taskMemAvailable {
				continue
			}
		case "recall_memory", "load_memory":
			if !taskMemAvailable || sawMemoryRecall {
				continue
			}
			sawMemoryRecall = true
		}
		names = append(names, t)
	}
	return names
}
