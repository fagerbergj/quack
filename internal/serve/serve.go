// Package serve is Quack's server bootstrap: config, inference, orchestrator, stores, REST + MCP + SPA.
package serve

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/acp"
	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/adkdebug"
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

// dotagentsEmbeddedSkills is the one plugin's skills/ subtree quack's own
// go:embed (embed.go) bakes into the binary. buildFromConfig hard-requires
// format-markdown and plan-work at startup, both shipped there, so a
// standalone install with no repo checkout (bundledir's disk-then-embedded
// resolution) must still be able to find them even though plugin discovery
// is otherwise disk-only. No other plugin gets this - it's the only one
// embedded.
const dotagentsEmbeddedSkills = ".agents/vendor/dotagents/skills"

// Where the plugin pins and their fetcher live, relative to CWD (/ in the
// image, the repo root in a dev run). Absent in a standalone install, which
// then reports on-disk revisions only.
const (
	pluginManifestPath = ".agents/vendor/plugins.yaml"
	pluginFetchScript  = "scripts/plugins.sh"
)

// resolvedSkillSource merges quack's own shipped skills/ with each configured
// plugin root's skills directory (internal/plugin discovery) - the disk-only
// view both newSkillSource and acpSkillPaths compare the embedded dotagents
// fallback against.
func resolvedSkillSource(skillDirs []string) skill.Source {
	sources := []skill.Source{skill.NewFileSystemSource(bundledir.SubFS("skills"))}
	for _, dir := range skillDirs {
		sources = append(sources, skill.NewFileSystemSource(os.DirFS(dir)))
	}
	return skill.NewMergedSource(sources...)
}

// missingDotagentsSkillNames returns the dotagentsEmbeddedSkills names NOT
// already resolved on disk via resolvedSkillSource(pluginRoots) - the backfill
// rule both newSkillSource and acpSkillPaths apply: add by NAME, not
// unconditionally, since a dotagents plugin root that DID resolve from disk
// must never also get the embedded copy (MergedSource errors on a skill
// defined by two sources at once).
func missingDotagentsSkillNames(skillDirs []string) []string {
	have := map[string]bool{}
	if fms, err := resolvedSkillSource(skillDirs).ListFrontmatters(context.Background()); err == nil {
		for _, fm := range fms {
			have[fm.Name] = true
		}
	}
	fallback := skill.NewFileSystemSource(bundledir.SubFS(dotagentsEmbeddedSkills))
	var missing []string
	if fms, err := fallback.ListFrontmatters(context.Background()); err == nil {
		for _, fm := range fms {
			if !have[fm.Name] {
				missing = append(missing, fm.Name)
			}
		}
	}
	return missing
}

// newSkillSource builds the skill toolset Source: resolvedSkillSource, then
// dotagentsEmbeddedSkills backfills any names discovery didn't resolve from
// disk, so a standalone install with no repo checkout still gets it.
func newSkillSource(skillDirs []string) skill.Source {
	resolved := resolvedSkillSource(skillDirs)
	backfill := missingDotagentsSkillNames(skillDirs)
	if len(backfill) == 0 {
		return resolved
	}
	fallback := skill.NewFileSystemSource(bundledir.SubFS(dotagentsEmbeddedSkills))
	return skill.NewMergedSource(resolved, skillsource.Scoped(fallback, backfill))
}

// ledgerStoreFromConfig resolves the replay ledger backend from stores, best-effort.
func ledgerStoreFromConfig(cfg *config.Config) ledger.LedgerStore {
	name := cfg.Observability.Recording.Store
	if name == "" {
		return nil
	}
	s, ok := cfg.Store(name)
	if !ok || s.Kind != "filesystem" {
		return nil
	}
	store, err := ledger.NewFSStore(s.Root)
	if err != nil {
		slog.Warn("replay ledger store init failed; recording disabled", "component", "startup", "err", err)
		return nil
	}
	return store
}

// setDefaultAgent stamps m's metrics-only agent fallback (tracedModel.SetDefaultAgent) -
// for any model or embedder consumer that never runs inside a DAG node's coords-stamped
// ctx. No-op for a value that doesn't implement it (e.g. under test).
func setDefaultAgent(m any, name string) {
	if da, ok := m.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent(name)
	}
}

// buildArtifactService resolves cfg.Artifacts: in-memory by default, or the
// durable Postgres large-object backend for a named store.
func buildArtifactService(cfg *config.Config) (artifact.Service, error) {
	if cfg.Artifacts.Store == "" {
		return artifact.InMemoryService(), nil
	}
	as, ok := cfg.Store(cfg.Artifacts.Store)
	if !ok {
		return nil, fmt.Errorf("artifacts store %q not found in stores registry", cfg.Artifacts.Store)
	}
	return store.NewArtifactService(as.URL)
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
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", nil, fmt.Errorf("config load failed: %w", err)
	}
	return InProcessFromConfig(ctx, cfg)
}

// InProcessFromConfig is InProcess for a caller with a resolved *config.Config in memory.
func InProcessFromConfig(ctx context.Context, cfg *config.Config) (baseURL string, stop func() error, err error) {
	setupLoggingTo(os.Stderr, slog.LevelWarn)
	handler, cleanup, _, err := buildFromConfig(ctx, cfg, 0, false, nil)
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

// shutdownHooks is an out-param for what Run needs post-build to drain
// SIGTERM (DrainActiveRuns) - avoids growing buildFromConfig's return arity
// across its ~25 early error returns. nil skips draining (InProcess's CLI use).
type shutdownHooks struct {
	hub *stream.Hub
	// pauser is the live executor: the drain pauses its running nodes rather
	// than cancelling the runs (#962).
	pauser nodePauser
	grace  time.Duration
}

// build loads config and constructs the HTTP handler, shared by Run and InProcess.
func build(ctx context.Context, configPath string, port int, reconcile bool, hooks *shutdownHooks) (handler http.Handler, cleanup func(), addr string, err error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("config load failed: %w", err)
	}
	return buildFromConfig(ctx, cfg, port, reconcile, hooks)
}

// buildFromConfig is build for an already-loaded Config. reconcile gates startup orphan reconciliation.
func buildFromConfig(ctx context.Context, cfg *config.Config, port int, reconcile bool, hooks *shutdownHooks) (handler http.Handler, cleanup func(), addr string, err error) {
	var cleanups []func()
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	defer func() {
		if err != nil {
			runCleanups()
			handler = nil
		}
	}()

	addr = cfg.Server.Addr
	if port != 0 {
		addr = fmt.Sprintf(":%d", port)
	}

	authMW, err := auth.New(cfg.Auth)
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth init failed: %w", err)
	}

	ledgerStore := ledgerStoreFromConfig(cfg)
	otelProviders, otelShutdown, err := otelobs.Init(ctx, cfg.Observability, ledgerStore, Version)
	if err != nil {
		return nil, nil, "", fmt.Errorf("otel init failed: %w", err)
	}
	cleanups = append(cleanups, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(sctx); err != nil {
			slog.Warn("otel shutdown failed", "component", "otelobs", "err", err)
		}
	})
	slog.SetDefault(slog.New(otelobs.WrapHandler(slog.Default().Handler())))

	go ledger.RunRetentionSweep(ctx, ledgerStore, cfg.Observability.Recording.RetentionDays, 24*time.Hour)

	jail, err := workspace.NewJail(cfg.Workspace.Root)
	if err != nil {
		return nil, nil, "", fmt.Errorf("workspace init failed: %w", err)
	}

	if cfg.Server.Managed() {
		if err = upStores(ctx); err != nil {
			return nil, nil, "", err
		}
		cleanups = append(cleanups, func() {
			slog.Info("managed stores left running; tear down with `docker compose -p quack-stores down`", "component", "serve")
		})
	}

	sessionStore, ok := cfg.Store(cfg.Session.Store)
	if !ok {
		return nil, nil, "", fmt.Errorf("session store %q not found in stores registry", cfg.Session.Store)
	}
	st, err := store.New(sessionStore.Kind, sessionStore.URL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("store open failed: %w", err)
	}
	var resumeNodes []store.ResumableNode
	if reconcile {
		id, err := store.LoadOrCreateInstanceID(cfg.Workspace.Root)
		if err != nil {
			return nil, nil, "", fmt.Errorf("instance id init failed: %w", err)
		}
		st.SetInstanceID(id)
		// Boot's half of #962. Runs here, before anything can register a run
		// with the Hub, and before the memory consolidator's own boot sweep
		// is started further down - resume gets the DB to a settled state
		// first, the sweep goroutines start after.
		resumeNodes = reconcileNodes(context.Background(), st, func(chatID string) (bool, string) {
			// A resumable node was provisioned a chat scope dir; if the
			// workspace is gone the run cannot pick up where it left off.
			if _, rerr := jail.Resolve(st.SessionUserForChat(context.Background(), chatID), chatID, "."); rerr != nil {
				return false, "workspace dir is gone"
			}
			return true, ""
		})
	}

	artifactSvc, err := buildArtifactService(cfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("artifact service init failed: %w", err)
	}
	artifacts := store.NewTurnAwareService(artifactSvc)
	st.SetArtifactService(artifacts)

	prov, _ := cfg.Provider(cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, cfg.Orchestrator.Model, artifacts)
	if err != nil {
		return nil, nil, "", fmt.Errorf("inference model init failed: %w", err)
	}
	// Never runs inside a DAG node, so no vetting.Config.Agent stamps it.
	setDefaultAgent(llm, orchestrator.AgentName)

	// runHub is needed by the SDK extensions built below (Dispatch fans a
	// run's events through it) as well as REST.
	runHub := stream.NewHub()
	if hooks != nil {
		hooks.hub = runHub
		hooks.grace = time.Duration(cfg.Server.ShutdownGraceSeconds) * time.Second
	}

	// orchRef/judgeModelRef are resolved further down - an SDK extension's
	// Dispatch/Classify may not fire until long after construction, but its
	// Tools() are needed now to fold into extTools before buildAgents (which
	// is also what actually builds the judge model judgeModelRef will hold).
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	var judgeModelRef atomic.Pointer[model.LLM]

	// Bring the plugin trees to their pinned refs before anything reads them,
	// and log what we actually got - skills change how every agent plans, so
	// the revision belongs in the startup record.
	pluginRevs := plugin.Refresh(pluginManifestPath, pluginFetchScript)
	if len(pluginRevs) > 0 {
		slog.Info("skill plugins resolved", "component", "startup", "revisions", plugin.Summary(pluginRevs))
	}

	// One resolution of the plugin roots drives all three component types.
	// The module and config checks run before anything is constructed, so a
	// manifest promising code this binary doesn't carry fails here, named.
	plugins, err := plugin.Resolve(cfg.PluginRoots())
	if err != nil {
		return nil, nil, "", err
	}
	if err := checkPluginModules(plugins); err != nil {
		return nil, nil, "", err
	}
	if err := checkPluginConfig(plugins, cfg.Extensions.Modules); err != nil {
		return nil, nil, "", err
	}

	pluginSkillDirs := plugin.SkillDirs(plugins)
	builtinSkillSrc := newSkillSource(pluginSkillDirs)
	builtinSkillSrc = workflowcatalog.Wrap(builtinSkillSrc, workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision))
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		return nil, nil, "", fmt.Errorf("skills toolset init failed: %w", err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}

	openMemory := func(rm config.ResolvedMemory, domain string) (*memory.Store, error) {
		eprov, ok := cfg.Provider(rm.Embedder.Provider)
		if !ok {
			return nil, fmt.Errorf("embedder provider %q not found", rm.Embedder.Provider)
		}
		embedder, err := inference.NewEmbedder(eprov, rm.Embedder.Model, artifacts)
		if err != nil {
			return nil, fmt.Errorf("embedder: %w", err)
		}
		// recall runs inside a DAG node's own ctx (real per-round coords); commit
		// fires from a background goroutine (commitMemoryOnPass) or a tool call
		// whose ctx never carries them - "embed" is the fallback for that gap,
		// distinct from the consolidator's "memory" name below.
		setDefaultAgent(embedder, "embed")
		cprov, ok := cfg.Provider(rm.Consolidation.Provider)
		if !ok {
			return nil, fmt.Errorf("consolidation provider %q not found", rm.Consolidation.Provider)
		}
		consolidator, err := inference.NewModel(cprov, rm.Consolidation.Model, artifacts)
		if err != nil {
			return nil, fmt.Errorf("consolidation model: %w", err)
		}
		// Commit runs from a background goroutine (user memory hook) or a tool call
		// whose ctx lost its node coords - never a DAG node's own model call.
		setDefaultAgent(consolidator, "memory")
		s, err := memory.New(context.Background(), rm.Kind, rm.URL, embedder, consolidator, rm.Collection, domain, rm.TopK, rm.MinScore)
		if err != nil {
			return nil, err
		}
		// internal/memory can't import internal/store; st (already open above) is
		// the memory_ops audit sink, wired in here.
		s.SetOpsLog(storeOpsLog{st})
		return s, nil
	}
	// Consolidation sweeps sweep on their first tick, immediately. Deferred
	// into this slice and started after the resumed nodes are dispatched, so
	// a boot resume never contends with #961's sweep for the same chat.
	var startSweeps []func()
	var taskStore, userStore *memory.Store
	if rm, ok := cfg.MemoryStore("stage_memory"); ok {
		s, err := openMemory(rm, "task")
		if err != nil {
			return nil, nil, "", fmt.Errorf("task memory init failed: %w", err)
		}
		taskStore = s
		slog.Info("semantic memory enabled", "component", "startup", "collection", rm.Collection,
			"embedder", rm.Embedder.Model, "consolidation", rm.Consolidation.Model)
		startSweeps = append(startSweeps, func() { startConsolidationSweep(ctx, s, rm) })
	}
	if slices.Contains(cfg.Orchestrator.Tools, "commit_memory") {
		if rm, ok := cfg.MemoryStore("commit_memory"); ok {
			s, err := openMemory(rm, "user")
			if err != nil {
				return nil, nil, "", fmt.Errorf("user memory init failed: %w", err)
			}
			userStore = s
			slog.Info("user memory enabled", "component", "startup", "collection", rm.Collection)
			startSweeps = append(startSweeps, func() { startConsolidationSweep(ctx, s, rm) })
		}
	}

	// Built after taskStore/userStore so UpdateChatOrigin's memory-outcome
	// mapping (design doc §4(b)/§5) can close over the concrete stores
	// instead of a lazily-resolved ref.
	sdkExts, err := buildSDKExtensions(cfg, st, runHub, &orchRef, artifacts, jail, &judgeModelRef, taskStore, userStore)
	if err != nil {
		return nil, nil, "", err
	}
	extTools := sdkExtensionTools(sdkExts)
	// Plugin-declared MCP servers are the portable half of the same tool
	// surface: out of process, jailed, and folded in by name like any other.
	if mcpCaps, err := pluginSpawnCaps(cfg, jail); err != nil {
		slog.Warn("plugin MCP servers skipped; sandbox caps unavailable", "component", "startup", "err", err)
	} else {
		extTools = append(extTools, pluginMCPTools(ctx, plugins, cfg.Workspace.Root, mcpCaps)...)
	}

	// The SDK inverse interfaces' first real consumer: whichever compiled,
	// configured module implements them (github, today) supplies quack's
	// push credential and delivery target - detected the same way
	// Starter/Stopper are, not hardcoded to one extension's name.
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

	cleanups = append(cleanups, func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		stopSDKExtensions(stopCtx, sdkExts)
	})

	var advisorAgent adkagent.Agent
	if cfg.Gates.JudgeEnabled() {
		if aprov, ok := cfg.Provider(cfg.Gates.Judge.Provider); ok {
			if am, merr := inference.NewModel(aprov, cfg.Gates.Judge.Model, artifacts); merr != nil {
				slog.Warn("advisor model build failed; ask_advisor disabled", "component", "startup", "err", merr)
			} else if ab, berr := agent.LoadBundle("agents/advisor"); berr != nil {
				slog.Warn("advisor bundle load failed; ask_advisor disabled", "component", "startup", "err", berr)
			} else if built, aerr := agent.BuildChat(ab, am, nil, nil, agent.Compaction{}, "", nil, ""); aerr != nil {
				slog.Warn("advisor build failed; ask_advisor disabled", "component", "startup", "err", aerr)
			} else {
				// ask_advisor runs the advisor as its own nested runner.Run - never a DAG node's own model call.
				setDefaultAgent(am, "advisor")
				advisorAgent = built
				slog.Info("advisor enabled", "component", "startup", "model", cfg.Gates.Judge.Model)
			}
		}
	}

	var executorRef atomic.Pointer[dag.Executor]
	nodeCancelled := func(chatID, nodeID string) bool {
		ex := executorRef.Load()
		return ex != nil && ex.NodeCancelled(chatID, nodeID)
	}

	var setupFn dag.SetupFunc
	clientMap, modelMap, nodeServers, judgeFactory, planJudge, gateCfgs, judgeModel, err := buildAgents(cfg, st.Sessions, skillTS, builtinSkillSrc, newScopedSkillTS, taskStore, advisorAgent, jail, gitTokenSource, extTools, pluginSkillDirs, deliver, nodeCancelled, &setupFn, artifacts)
	if err != nil {
		return nil, nil, "", fmt.Errorf("agent build failed: %w", err)
	}
	if judgeModel != nil {
		judgeModelRef.Store(&judgeModel)
	}
	cleanups = append(cleanups, func() {
		nodeServers.closeAll()
	})

	agentInfos := make([]dag.AgentInfo, 0, len(clientMap))
	mediaAgents := make(map[string]bool)
	for name, c := range clientMap {
		ac := cfg.Agents[name]
		agentInfos = append(agentInfos, dag.AgentInfo{Name: name, Description: c.Description(), ContextWindow: ac.ContextWindow})
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

	orchBundle, err := agent.LoadBundle("agents/orchestrator")
	if err != nil {
		return nil, nil, "", fmt.Errorf("orchestrator bundle load failed: %w", err)
	}
	fmFm, err := skillSrc.LoadFrontmatter(context.Background(), "format-markdown")
	if err != nil {
		return nil, nil, "", fmt.Errorf("format-markdown skill load failed: %w", err)
	}
	planWorkFm, err := skillSrc.LoadFrontmatter(context.Background(), "plan-work")
	if err != nil {
		return nil, nil, "", fmt.Errorf("plan-work skill load failed: %w", err)
	}
	orchBehaviour := orchBundle.Prompt
	if userStore != nil {
		mem, err := agent.LoadBundleMemory("agents/orchestrator")
		if err != nil {
			return nil, nil, "", fmt.Errorf("orchestrator memory.md load failed: %w", err)
		}
		if mem != "" {
			orchBehaviour += "\n\n" + mem
		}
	}
	orchSysPrompt := promptbuilder.Orchestrator(rosterSB.String(), []*skill.Frontmatter{fmFm, planWorkFm}, orchBehaviour)

	orchSkillTS, err := newScopedSkillTS(cfg.Orchestrator.Skills)
	if err != nil {
		return nil, nil, "", fmt.Errorf("orchestrator skills toolset init failed: %w", err)
	}

	planner := dag.NewPlanner(agentInfos, cfg.Workspace.CheckCommands, planJudge)
	cfgFor := func(name string) vetting.Config { return gateCfgs[name] }
	executor := dag.NewExecutor(st.Sessions, clientMap, modelMap, judgeFactory, cfgFor, mediaAgents)
	executor.SetMaxActive(cfg.Dag.MaxActiveNodes)
	executor.SetSetup(setupFn)
	executor.SetArtifacts(artifacts)
	executor.SetNodeStateStore(st) // write-through node state machine (#962)
	executorRef.Store(executor)
	orch := orchestrator.New(st.Sessions, llm, orchSysPrompt, planner, executor, orchSkillTS, userStore, taskStore)
	orch.SetMaxActiveRuns(cfg.Dag.MaxActiveRuns)
	orchRef.Store(orch)
	if hooks != nil {
		hooks.pauser = executor
	}
	// After the orchestrator exists: re-enter each resumed node's graph. The
	// store-side reconcile already ran at boot, so a crash here leaves the
	// nodes paused and the next boot picks them up again.
	startResumedNodes(ctx, resumeNodes, orch, st, runHub)
	for _, start := range startSweeps {
		start()
	}
	if err := startSDKExtensions(ctx, sdkExts); err != nil {
		return nil, nil, "", fmt.Errorf("sdk extensions start failed: %w", err)
	}

	if userStore != nil && cfg.Orchestrator.UserMemoryHook.Enabled {
		if memAgent, err := buildUserMemoryHookAgent(cfg.Orchestrator.UserMemoryHook, cfg, artifacts); err != nil {
			slog.Warn("user memory hook build failed; hook disabled", "component", "startup", "err", err)
		} else {
			orch.SetUserMemoryHook(memAgent)
			slog.Info("user memory hook enabled", "component", "startup", "model", cfg.Orchestrator.UserMemoryHook.Model)
		}
	}

	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, nil, "", fmt.Errorf("embed SPA fs failed: %w", err)
	}

	var adkDebugHandler http.Handler
	if cfg.Observability.ADKDebug {
		if mount, derr := adkdebug.New(st.Sessions, clientMap, artifacts); derr != nil {
			slog.Warn("adk debug mount failed; disabled", "component", "startup", "err", derr)
		} else {
			if otelProviders.TracerProvider != nil {
				otelProviders.TracerProvider.RegisterSpanProcessor(mount.SpanProcessor())
			} else {
				slog.Warn("adk debug mount enabled but otel is disabled; /debug/trace will stay empty", "component", "startup")
			}
			adkDebugHandler = mount.Handler
			slog.Warn("ADK debug surface mounted - runs agents WITHOUT quack's trust gate; dev/trusted use only",
				"component", "startup", "path", adkdebug.MountPath)
		}
	}

	handler = server.New(server.Options{
		REST:          rest.NewHandler(st, orch, llm, jail, runHub, ledgerStore, Version, taskStore, userStore, artifacts, extensionDescriptors(sdkExts)),
		MCP:           mcpserver.Handler(orch),
		SPA:           spa,
		SDKExtensions: sdkExtensionMounts(sdkExts),
		Auth:          authMW,
		ADKDebug:      adkDebugHandler,
	})

	gcHomeDir, err := jail.HomeDir(localUserID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("workspace gc home dir init failed: %w", err)
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
	go workspace.RunGC(ctx, jail, gcCfg, runHub.HasRegisteredRun, func(pctx context.Context, dir string) error {
		return tools.PruneWorktree(pctx, dir, gcCaps)
	})

	return handler, runCleanups, addr, nil
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
func buildUserMemoryHookAgent(h config.UserMemoryHookConfig, cfg *config.Config, artifacts artifact.Service) (adkagent.Agent, error) {
	prov, ok := cfg.Provider(h.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q not found", h.Provider)
	}
	m, err := inference.NewModel(prov, h.Model, artifacts)
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	// Fires from a fire-and-forget goroutine after the orchestrator's own turn ends,
	// via its own nested runner.Run - never a DAG node's own model call.
	setDefaultAgent(m, "memory-hook")
	b, err := agent.LoadBundle("agents/memory-agent")
	if err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	whatToRemember, err := agent.LoadBundleMemory("agents/orchestrator")
	if err != nil {
		return nil, fmt.Errorf("orchestrator memory.md: %w", err)
	}
	rubric, err := vetting.LoadBundleRubric("agents/memory-agent")
	if err != nil {
		return nil, fmt.Errorf("rubric.md: %w", err)
	}
	guidance := strings.TrimSpace(whatToRemember + "\n\n" + rubric)
	return agent.BuildChat(b, m, nil, nil, agent.Compaction{}, guidance, nil, "")
}

// gitCredentialAdapter bridges tools.GitTokenSource to vetting.GitCredentialSource -
// vetting can't import internal/tools (tools already imports vetting), so it declares its own type.
type gitCredentialAdapter struct{ src tools.GitTokenSource }

func (a gitCredentialAdapter) GitCredential(ctx context.Context, rawURL string) (*vetting.GitCredential, error) {
	c, err := a.src.GitCredential(ctx, rawURL)
	if err != nil || c == nil {
		return nil, err
	}
	return &vetting.GitCredential{Host: c.Host, Username: c.Username, Token: c.Token}, nil
}

// buildAgents loads each agent bundle, builds its model and tools, exposes over A2A, returns client map.
func buildAgents(cfg *config.Config, sessions session.Service, skillTS *skilltoolset.SkillToolset, builtinSkillSrc skill.Source, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), taskStore *memory.Store, advisorAgent adkagent.Agent, jail *workspace.Jail, gitTokenSource tools.GitTokenSource, extTools []extTool, pluginSkillDirs []string, deliver vetting.DeliverFunc, nodeCancelled func(chatID, nodeID string) bool, setupOut *dag.SetupFunc, artifacts artifact.Service) (map[string]adkagent.Agent, map[string]model.LLM, *perNodeServers, vetting.JudgeFactory, vetting.PlanJudge, map[string]vetting.Config, model.LLM, error) {
	nodeServers := newPerNodeServers()

	nodeScope := func(ctx context.Context) memory.Scope {
		uc, ok := ctx.(interface{ UserContent() *genai.Content })
		if !ok {
			return memory.Scope{}
		}
		token, ok := vetting.ParseAdvisorThread(contentText(uc.UserContent()))
		if !ok {
			return memory.Scope{}
		}
		at, ok := vetting.LookupAdvisorThread(token)
		if !ok {
			return memory.Scope{}
		}
		sc := memory.Scope{User: at.UserID}
		if jail != nil {
			sc.Repo = jail.RepoKey(localUserID, at.SessionID)
		}
		return sc
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	urlCache := tools.NewURLCache()

	workspaceCaps := workspace.Caps{
		MaxReadBytes:   int64(cfg.Workspace.MaxReadKB) * 1024,
		MaxWriteBytes:  int64(cfg.Workspace.MaxWriteKB) * 1024,
		MaxResults:     cfg.Workspace.MaxResults,
		MaxListEntries: cfg.Workspace.MaxListEntries,
		Timeout:        time.Duration(cfg.Workspace.TimeoutSeconds) * time.Second,
		ExtraPath:      cfg.Workspace.ExecPath,
		Env:            cfg.Workspace.Env,
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

	var judgeFactory vetting.JudgeFactory
	var planJudge vetting.PlanJudge
	var gateCfg vetting.Config
	var judgeModel model.LLM
	var safetyJudge tools.SafetyJudge
	if cfg.Gates.Enabled() {
		var err error
		if gateCfg, err = vetting.FromConfig(cfg.Gates); err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, err
		}
		gateCfg.Memory = taskStore
		gateCfg.Workspace = jail
		gateCfg.WorkspaceUserID = localUserID
		gateCfg.WorkspaceCaps = workspaceCaps
		gateCfg.Deliver = deliver
		if gitTokenSource != nil {
			gateCfg.GitCredentials = gitCredentialAdapter{gitTokenSource}
		}
		gateCfg.CheckCommands = cfg.Workspace.CheckCommands
		gateCfg.CheckSetup = cfg.Workspace.CheckSetup
		if cfg.Gates.JudgeEnabled() {
			jprov, ok := cfg.Provider(cfg.Gates.Judge.Provider)
			if !ok {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmt.Errorf("gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
			}
			judge, err := inference.NewModel(jprov, cfg.Gates.Judge.Model, artifacts)
			if err != nil {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmt.Errorf("gates.judge: model: %w", err)
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
					return nil, nil, nodeServers, nil, nil, nil, nil, fmt.Errorf("gates.judge: read tools: %w", err)
				}
			}
			var judgeSkillsets []tool.Toolset
			if skillTS != nil {
				judgeSkillsets = []tool.Toolset{skillTS}
			}
			judgeFactory = vetting.NewJudgeFactory(judge, judgeReadTools, judgeSkillsets)
			if cfg.Gates.Judge.Skeptics > 0 {
				gateCfg.Skeptic = vetting.NewSkepticFactory(judge, judgeReadTools)
			}
			safetyJudge = tools.NewSafetyJudge(judge)
			planJudge = vetting.NewPlanJudge(judge, cfg.Gates.Judge.MaxOutputTokens)
		}
		slog.Info("trust gate enabled", "component", "startup",
			"deterministic_rounds", gateCfg.DeterministicRounds,
			"judge", cfg.Gates.Judge.Model, "judge_rounds", gateCfg.JudgeRounds, "threshold", gateCfg.Threshold)
	}

	gitCredentials := make([]tools.GitCredential, len(cfg.Workspace.GitCredentials))
	for i, gc := range cfg.Workspace.GitCredentials {
		gitCredentials[i] = tools.GitCredential{Host: gc.Host, Username: gc.Username, Token: gc.Token}
	}
	if setupOut != nil {
		*setupOut = func(ctx context.Context, _, chatID, dir string, setup dag.Setup) error {
			_, err := tools.SetupClone(ctx, jail, localUserID, chatID, dir, setup.Repo, setup.BaseRef, setup.WorkBranch, setup.CheckoutExistingHead, workspaceCaps, gitCredentials, gitTokenSource, cfg.Workspace.CheckSetup)
			return err
		}
	}

	var fallbackSummarizer model.LLM
	compCfg := cfg.Session.Compaction
	if compCfg.Enabled && compCfg.Model != "" {
		cprov, ok := cfg.Provider(compCfg.Provider)
		if !ok {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmt.Errorf("compaction: provider %q not found", compCfg.Provider)
		}
		var err error
		if fallbackSummarizer, err = inference.NewModel(cprov, compCfg.Model, artifacts); err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmt.Errorf("compaction: model: %w", err)
		}
		// Only ever used when ResolveSummarizer has no active worker model - rare, but
		// that call site is outside any node's own coords stamp when it happens.
		setDefaultAgent(fallbackSummarizer, "compaction")
		slog.Info("context compaction enabled", "component", "startup", "fallback_summariser", compCfg.Model)
	} else if compCfg.Enabled {
		slog.Info("context compaction enabled", "component", "startup", "summariser", "active worker model (no fallback configured)")
	}
	compactionFor := func(ac config.AgentConfig, workerModel model.LLM) agent.Compaction {
		if !compCfg.Enabled {
			return agent.Compaction{}
		}
		if ac.ContextWindow == 0 {
			slog.Warn("context compaction: agent has no context_window configured; not compacting it", "component", "startup", "model", ac.Model)
			return agent.Compaction{}
		}
		return agent.Compaction{
			Summarizer:         agent.ResolveSummarizer(workerModel, fallbackSummarizer),
			ContextWindow:      ac.ContextWindow,
			Enabled:            true,
			Sessions:           sessions,
			TokenThreshold:     compCfg.TokenThreshold,
			EventRetentionSize: compCfg.EventRetentionSize,
		}
	}

	clientMap := make(map[string]adkagent.Agent, len(cfg.Agents))
	modelMap := make(map[string]model.LLM, len(cfg.Agents))
	gateCfgs := make(map[string]vetting.Config, len(cfg.Agents))

	// Plugin MCP tool names come from third-party mcp.json authors, so a
	// collision with an SDK extension's tool is plausible and silent -
	// indexExtTools prefixes colliding names and makes bare use an error.
	extToolsByName := indexExtTools(extTools)

	for _, name := range names {
		ac := cfg.Agents[name]

		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "provider %q not found", ac.Provider)
		}
		m, err := inference.NewModel(prov, ac.Model, artifacts)
		if err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "model: %v", err)
		}
		// m's pricing (above) never surfaces - ACP's workerModel is never invoked,
		// so resolve pricing again here the way inference.NewModel does.
		var acpPricing *config.ModelPricing
		if pr, ok := prov.Models[ac.Model]; ok {
			acpPricing = &pr
		}

		if ac.Acp != nil {
			bundle, err := agent.LoadBundle(ac.Bundle)
			if err != nil {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "bundle: %v", err)
			}
			var memGuidance string
			if taskStore != nil {
				if memGuidance, err = agent.LoadBundleMemory(ac.Bundle); err != nil {
					return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "memory.md: %v", err)
				}
			}
			var grading string
			if cfg.Gates.Enabled() && ac.IsGated() {
				agentGateCfg, err := perAgentGateCfg(gateCfg, name, ac, taskStore != nil, memGuidance)
				if err != nil {
					return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "rubric: %v", err)
				}
				gateCfgs[name] = agentGateCfg
				grading = promptbuilder.GradingFacts(agentGateCfg.Threshold, agentGateCfg.JudgeRounds, agentGateCfg.ReadOnly, agentGateCfg.RequireRetrieval)
			}
			skillFms, err := builtinSkillSrc.ListFrontmatters(context.Background())
			if err != nil {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "skills: %v", err)
			}
			behaviour := bundle.Prompt
			if g := strings.TrimSpace(memGuidance); g != "" {
				behaviour += "\n\n" + g
			}
			wsBlock := workspace.PromptBlock(workspaceCaps, cfg.Workspace.CheckCommands)
			preamble := promptbuilder.Agent(bundle.Card.Name, bundle.Card.Description, nil, skillFms, behaviour, grading, wsBlock)
			env := opencodeEnv(prov, ac, acpSkillPaths(pluginSkillDirs), workspaceCaps)
			env = append(env, acpChildEnv(cfg.Workspace.Env, ac.Acp.Env)...)
			var permJudge func(ctx context.Context, toolName, title string, input map[string]any) (bool, string)
			if safetyJudge != nil {
				sj := safetyJudge
				agentName := name
				permJudge = func(ctx context.Context, toolName, title string, input map[string]any) (bool, string) {
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
			var acpReplay *replay.Session
			if prov.Kind == "replay" {
				acpReplay, err = replay.Load(prov.Bundle)
				if err != nil {
					return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "acp replay: %v", err)
				}
				if prov.ForkMode == "fork" {
					acpReplay.EnableFork(prov.ForkFrom)
				}
			}
			ag, err := acp.New(name, bundle.Card.Description, acp.Options{
				Command:         ac.Acp.Command,
				Env:             env,
				Replay:          acpReplay,
				Caps:            workspaceCaps,
				ExtraRO:         acpSkillPaths(pluginSkillDirs),
				Home:            workspaceCaps.HomeDir,
				Preamble:        preamble,
				Jail:            jail,
				UserID:          localUserID,
				PermissionJudge: permJudge,
				ModelName:       ac.Model,
				Pricing:         acpPricing,
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
				return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "acp: %v", err)
			}
			clientMap[name] = ag
			modelMap[name] = m
			slog.Info("agent running via ACP subprocess", "component", "startup",
				"agent", name, "command", strings.Join(ac.Acp.Command, " "), "model", ac.Model)
			continue
		}

		toolNames, wantLoadMemory := resolveToolNames(ac.Tools, taskStore != nil, advisorAgent != nil)

		bundle, err := agent.LoadBundle(ac.Bundle)
		if err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "bundle: %v", err)
		}
		var memGuidance string
		if taskStore != nil {
			if memGuidance, err = agent.LoadBundleMemory(ac.Bundle); err != nil {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "memory.md: %v", err)
			}
		}
		var memSvc adkmemory.Service
		if taskStore != nil && memGuidance != "" {
			memSvc = taskStore.View(memory.Scope{Role: ac.Memory.Bucket, Legacy: name}, nodeScope)
		}
		agentSkillTS, err := newScopedSkillTS(ac.Skills)
		if err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "skills toolset: %v", err)
		}
		skillFms, err := skillsource.Scoped(builtinSkillSrc, ac.Skills).ListFrontmatters(context.Background())
		if err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "skills: %v", err)
		}

		if cfg.Gates.Enabled() && !ac.IsGated() {
			slog.Info("trust gate skipped for agent (gated: false)", "component", "startup", "agent", name)
		}
		var grading string
		if cfg.Gates.Enabled() && ac.IsGated() {
			agentGateCfg, err := perAgentGateCfg(gateCfg, name, ac, taskStore != nil, memGuidance)
			if err != nil {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "rubric: %v", err)
			}
			gateCfgs[name] = agentGateCfg
			grading = promptbuilder.GradingFacts(agentGateCfg.Threshold, agentGateCfg.JudgeRounds, agentGateCfg.ReadOnly, agentGateCfg.RequireRetrieval)
		}

		var nativeReplay *replay.Session
		if prov.Kind == "replay" {
			if nativeReplay, err = replay.Load(prov.Bundle); err != nil {
				return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "replay: tools: %v", err)
			}
			if prov.ForkMode == "fork" {
				nativeReplay.EnableFork(prov.ForkFrom)
			}
		}

		buildWorker := func() (adkagent.Agent, model.LLM, []tool.Tool, error) {
			wm, err := inference.NewModel(prov, ac.Model, artifacts)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("model: %w", err)
			}
			var builtins []tool.Tool
			if len(toolNames) > 0 {
				if builtins, err = tools.Build(toolNames, tools.Deps{
					WebSearch:       tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL, Key: cfg.Tools["web_search"].APIKey()},
					Fetch:           tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
					Summarizer:      wm,
					Cache:           urlCache,
					Advisor:         advisorAgent,
					Sessions:        sessions,
					Workspace:       jail,
					WorkspaceUserID: localUserID,
					WorkspaceCaps:   workspaceCaps,
					GitCredentials:  gitCredentials,
					GitTokenSource:  gitTokenSource,
					Guards:          cfg.Workspace.Guards,
					SafetyJudge:     safetyJudge,
					NodeCancelled:   nodeCancelled,
					ExtTools:        extToolsByName,
					Replayer:        nativeReplay,
				}); err != nil {
					return nil, nil, nil, fmt.Errorf("tools: %w", err)
				}
			}
			if memSvc != nil {
				builtins = append(builtins, memory.NewPreload())
				if wantLoadMemory {
					builtins = append(builtins, loadmemorytool.New())
				}
			}
			comp := compactionFor(ac, wm)
			wag, err := agent.Build(bundle, wm, builtins, []tool.Toolset{agentSkillTS}, comp, memGuidance, skillFms, grading)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("build: %w", err)
			}
			return wag, wm, builtins, nil
		}

		protoAgent, _, _, err := buildWorker()
		if err != nil {
			return nil, nil, nodeServers, nil, nil, nil, nil, fmtErr(name, "%v", err)
		}
		clientMap[name] = nativeAgent{
			Agent: protoAgent,
			build: func(nodeKey string) (adkagent.Agent, model.LLM, []tool.Tool, func(), error) {
				wag, wm, builtins, err := buildWorker()
				if err != nil {
					return nil, nil, nil, nil, err
				}
				srv, err := agent.Serve(wag, sessions, memSvc)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("a2a serve: %w", err)
				}
				client, err := srv.ClientForNode(nodeKey)
				if err != nil {
					_ = srv.Close()
					return nil, nil, nil, nil, fmt.Errorf("a2a client: %w", err)
				}
				return client, wm, builtins, nodeServers.track(srv), nil
			},
		}
		slog.Info("agent serving over A2A per DAG node", "component", "startup", "agent", name, "tools", ac.Tools)
	}
	return clientMap, modelMap, nodeServers, judgeFactory, planJudge, gateCfgs, judgeModel, nil
}

// perAgentGateCfg specializes the base trust-gate config for one agent.
func perAgentGateCfg(base vetting.Config, name string, ac config.AgentConfig, memEnabled bool, memGuidance string) (vetting.Config, error) {
	c := base
	c.CommitMemory = memEnabled && memGuidance != ""
	if memEnabled && ac.Memory.Bucket != "" && memGuidance == "" {
		slog.Warn("agent has a memory bucket but no memory.md; it will recall but never commit",
			"component", "serve", "agent", name, "bucket", ac.Memory.Bucket, "bundle", ac.Bundle)
	}
	c.MemoryRole = ac.Memory.Bucket
	for _, tn := range ac.Tools {
		if tn == "web_search" || tn == "web_fetch" {
			c.RequireRetrieval = true
			break
		}
	}
	c.ReadOnly = true
	for _, tn := range ac.Tools {
		if tn == "git_push" {
			c.ReadOnly = false
			break
		}
	}
	if ac.Acp != nil {
		c.ReadOnly = ac.Acp.ReadOnly
		c.ExternalWorker = true
	}
	if override, specs, fixes, err := vetting.LoadBundleRubricSpecs(ac.Bundle); err != nil {
		return c, err
	} else if override != "" {
		c.Rubric = override
		c.RubricSpecs = specs
		c.RubricFixes = fixes
		slog.Info("using per-agent rubric from bundle", "component", "startup", "agent", name)
	}
	if ac.JudgeRounds > 0 {
		c.JudgeRounds = ac.JudgeRounds
	}
	if ac.Judge != nil && !*ac.Judge {
		c.JudgeRounds = 0
	}
	slog.Info("per-agent trust gate config", "component", "startup", "agent", name, "judge_rounds", c.JudgeRounds)
	return c, nil
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

// opencodeEnv generates OPENCODE_CONFIG_CONTENT for an ACP agent: provider, model, headless permission policy.
func opencodeEnv(prov config.ProviderConfig, ac config.AgentConfig, skillPaths []string, caps workspace.Caps) []string {
	type m = map[string]any
	apiKey := prov.APIKey
	if apiKey == "" {
		apiKey = "unused"
	}
	modelCfg := m{}
	if ac.ContextWindow > 0 {
		modelCfg["limit"] = m{"context": ac.ContextWindow, "output": 32768}
	}
	// git push is left allowed here (#936): the ACP child's spawnEnv
	// (internal/acp.spawnEnv) strips its authority to authenticate to any real
	// remote (GIT_ASKPASS/GIT_SSH_COMMAND=/bin/false, GIT_TERMINAL_PROMPT=0), so
	// denying the command outright is no longer needed to keep delivery
	// gate-owned - it only broke the project's own tests, which push to a local
	// test remote. Clone is still denied (#579) except for a read-only
	// acp.allow_clone agent, which is chartered to read third-party repos the
	// gate never provisions. Clone plus the wide external_directory it needs
	// (the clone lands outside cwd, #346) rest on the RO work tree, so clone is
	// allowed only in a mode that OS-enforces it on the ACP child - landlock or
	// bwrap, both of which WrapArgv wraps (#921). Under `none` there is no
	// boundary at all, so allow_clone degrades to denied rather than to
	// unbounded.
	allowClone := ac.Acp != nil && ac.Acp.AllowClone && workspace.EnforcesBoundary(caps.Sandbox)
	if ac.Acp != nil && ac.Acp.AllowClone && !allowClone {
		slog.Warn("acp.allow_clone ignored: clone needs the read-only work tree OS-enforced, which sandbox: none cannot do for the ACP child",
			"component", "acp", "sandbox", caps.Sandbox)
	}
	bash := m{
		"*": "allow",
	}
	// external_directory governs opencode's native write/edit tool, not bash -
	// bash writes already reach $TMPDIR/opencode and caps.HomeDir (the OS
	// filesystem permits it), but this map was `deny` for both, so the native
	// tool disagreed with the environment block that calls them writable
	// (#949). "**" (not "*") matches across path separators, since opencode's
	// matcher treats "*" the way most globs do - opencode/* did not match
	// opencode/probe/…, only opencode's direct children.
	extDir := m{"*": "deny"}
	if allowClone {
		extDir = m{"*": "allow"}
	} else {
		bash["git clone"] = "deny"
		bash["git clone *"] = "deny"
		bash["gh repo clone"] = "deny"
		bash["gh repo clone *"] = "deny"
		if tmp := workspace.SandboxTmpDir(caps); tmp != "" {
			extDir[tmp+"/**"] = "allow"
		}
		if caps.HomeDir != "" {
			extDir[caps.HomeDir+"/**"] = "allow"
		}
	}
	cfg := m{
		"provider": m{"quack": m{
			"npm":     "@ai-sdk/openai-compatible",
			"name":    "quack-bound provider",
			"options": m{"baseURL": prov.Endpoint, "apiKey": apiKey},
			"models":  m{ac.Model: modelCfg},
		}},
		"model": "quack/" + ac.Model,
		"permission": m{
			"bash":               bash,
			"external_directory": extDir,
			"doom_loop":          "deny",
			"read":               m{"*.env": "deny", "*.env.*": "deny"},
		},
	}
	if len(skillPaths) > 0 {
		cfg["skills"] = m{"paths": skillPaths}
	}
	if len(ac.Acp.McpServers) > 0 {
		servers := m{}
		for i, u := range ac.Acp.McpServers {
			servers[mcpServerName(u, i)] = m{"type": "remote", "url": u, "enabled": true}
		}
		cfg["mcp"] = servers
	}
	content, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	return []string{"OPENCODE_CONFIG_CONTENT=" + string(content)}
}

// mcpServerName derives a config key from an MCP URL's registrable domain.
func mcpServerName(raw string, i int) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Sprintf("mcp-%d", i)
	}
	labels := strings.Split(u.Hostname(), ".")
	if n := len(labels); n >= 2 {
		return labels[n-2]
	}
	return labels[0]
}

// extractedDotagentsSkillsDir is where the embedded dotagents skills are
// materialised on disk for the sandboxed ACP child (opencode reads
// skills.paths off disk; it has no access to the binary's embedded FS,
// unlike the in-process skill toolset newSkillSource feeds). Under
// os.TempDir(), not caps.HomeDir: extraction happens once at startup, before
// any agent's per-round Caps exist, and both sandbox backends (bwrap's
// extraROArgs, landlock's landlockGrants) ro-bind/grant Caps.ExtraRO entries
// by absolute path with no same-device requirement - unlike TMPDIR (#939),
// this needs no device care.
var extractedDotagentsSkillsDir = filepath.Join(os.TempDir(), "quack-acp-dotagents-skills")

var extractDotagentsSkillsMu sync.Mutex

// ensureExtractedDotagentsSkillNames materialises exactly the named
// dotagentsEmbeddedSkills subdirectories under extractedDotagentsSkillsDir -
// per-skill, so a name that resolves from disk after having once been
// missing (a config change) doesn't leave a stale extracted duplicate
// alongside it. Idempotent: a name already extracted (checked by presence)
// is left alone, so restarts don't re-copy the whole tree. Extraction
// failure logs and degrades - a name that fails to extract is simply absent
// from the dir, same as a plugin-root skills dir that doesn't resolve.
func ensureExtractedDotagentsSkillNames(missing []string) {
	extractDotagentsSkillsMu.Lock()
	defer extractDotagentsSkillsMu.Unlock()

	want := map[string]bool{}
	for _, n := range missing {
		want[n] = true
	}
	if err := os.MkdirAll(extractedDotagentsSkillsDir, 0o755); err != nil {
		slog.Warn("acp skill extraction: could not create dir; ACP agents may miss dotagents skills",
			"component", "serve", "dir", extractedDotagentsSkillsDir, "err", err)
		return
	}
	// Prune anything extracted that's no longer missing, so it can't sit
	// alongside an on-disk copy of the same name.
	if entries, err := os.ReadDir(extractedDotagentsSkillsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !want[e.Name()] {
				_ = os.RemoveAll(filepath.Join(extractedDotagentsSkillsDir, e.Name()))
			}
		}
	}
	src := bundledir.SubFS(dotagentsEmbeddedSkills)
	for name := range want {
		dest := filepath.Join(extractedDotagentsSkillsDir, name)
		if _, err := os.Stat(dest); err == nil {
			continue // already extracted
		}
		sub, err := fs.Sub(src, name)
		if err != nil {
			slog.Warn("acp skill extraction: skill not found in embedded FS",
				"component", "serve", "skill", name, "err", err)
			continue
		}
		if err := os.CopyFS(dest, sub); err != nil {
			slog.Warn("acp skill extraction failed for one skill; ACP agents may miss it",
				"component", "serve", "skill", name, "err", err)
			_ = os.RemoveAll(dest)
		}
	}
}

// acpSkillPaths collects on-disk skill roots for an ACP agent's skills.paths:
// quack's own skills/, then each configured plugin's skills directory
// (internal/plugin discovery), then - only for names that resolution didn't
// find on disk - the extracted dotagentsEmbeddedSkills dir. Same by-NAME
// backfill rule as newSkillSource (missingDotagentsSkillNames): a dev run
// with dotagents checked out on disk must not also get the extracted copy,
// since opencode's skill loader may error (or worse, silently shadow) on a
// duplicate name.
func acpSkillPaths(skillDirs []string) []string {
	var out []string
	if abs, err := filepath.Abs("skills"); err == nil {
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			out = append(out, abs)
		}
	}
	out = append(out, skillDirs...)

	if missing := missingDotagentsSkillNames(skillDirs); len(missing) > 0 {
		ensureExtractedDotagentsSkillNames(missing)
		if st, err := os.Stat(extractedDotagentsSkillsDir); err == nil && st.IsDir() {
			out = append(out, extractedDotagentsSkillsDir)
		}
	}
	return out
}

// contentText flattens a content's text parts (for advisor-thread marker extraction).
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func fmtErr(agentName, format string, args ...any) error {
	return fmt.Errorf("agent %q: "+format, append([]any{agentName}, args...)...)
}

// resolveToolNames splits configured tool names into builtins and whether load_memory was requested.
func resolveToolNames(configured []string, taskMemAvailable, advisorAvailable bool) (names []string, wantLoadMemory bool) {
	names = make([]string, 0, len(configured))
	for _, t := range configured {
		switch t {
		case "load_memory":
			wantLoadMemory = true
			continue
		case "stage_memory":
			if !taskMemAvailable {
				continue
			}
		case "ask_advisor":
			if !advisorAvailable {
				continue
			}
		}
		names = append(names, t)
	}
	return names, wantLoadMemory
}
