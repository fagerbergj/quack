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
	"github.com/fagerbergj/quack/internal/extension"
	"github.com/fagerbergj/quack/internal/github"
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

// newSkillSource builds the skill toolset Source from quack's own shipped
// skills plus each configured plugin root's skills directory (internal/plugin
// discovery), in order. dotagentsEmbeddedSkills then backfills any names
// discovery didn't resolve from disk, so a standalone install with no repo
// checkout still gets it - added by NAME, not unconditionally: MergedSource
// errors on a skill defined by two sources at once, so a dotagents plugin
// root that DID resolve from disk must never also get the embedded copy.
func newSkillSource(pluginRoots []string) skill.Source {
	sources := []skill.Source{skill.NewFileSystemSource(bundledir.SubFS("skills"))}
	for _, dir := range plugin.ResolveSkillDirs(pluginRoots) {
		sources = append(sources, skill.NewFileSystemSource(os.DirFS(dir)))
	}
	resolved := skill.NewMergedSource(sources...)

	have := map[string]bool{}
	if fms, err := resolved.ListFrontmatters(context.Background()); err == nil {
		for _, fm := range fms {
			have[fm.Name] = true
		}
	}
	fallback := skill.NewFileSystemSource(bundledir.SubFS(dotagentsEmbeddedSkills))
	var backfill []string
	if fms, err := fallback.ListFrontmatters(context.Background()); err == nil {
		for _, fm := range fms {
			if !have[fm.Name] {
				backfill = append(backfill, fm.Name)
			}
		}
	}
	if len(backfill) == 0 {
		return resolved
	}
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
	handler, cleanup, addr, err := build(ctx, configPath, port, true)
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
	handler, cleanup, _, err := buildFromConfig(ctx, cfg, 0, false)
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

// build loads config and constructs the HTTP handler, shared by Run and InProcess.
func build(ctx context.Context, configPath string, port int, reconcile bool) (handler http.Handler, cleanup func(), addr string, err error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("config load failed: %w", err)
	}
	return buildFromConfig(ctx, cfg, port, reconcile)
}

// buildFromConfig is build for an already-loaded Config. reconcile gates startup orphan reconciliation.
func buildFromConfig(ctx context.Context, cfg *config.Config, port int, reconcile bool) (handler http.Handler, cleanup func(), addr string, err error) {
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
	otelProviders, otelShutdown, err := otelobs.Init(ctx, cfg.Observability, ledgerStore)
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
	if reconcile {
		id, err := store.LoadOrCreateInstanceID(cfg.Workspace.Root)
		if err != nil {
			return nil, nil, "", fmt.Errorf("instance id init failed: %w", err)
		}
		st.SetInstanceID(id)
		if n, err := st.FailStaleDagNodes(context.Background()); err != nil {
			slog.Error("fail stale dag nodes", "component", "store", "err", err)
		} else if n > 0 {
			slog.Info("marked orphaned dag nodes failed (previous process killed mid-run)", "component", "store", "count", n)
		}
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

	var githubApp *github.App
	var extTools []tool.Tool
	var gitTokenSource tools.GitTokenSource
	if gh := cfg.Extensions.GitHub; gh != nil {
		pem, kerr := github.LoadPrivateKey(gh.PrivateKey, gh.PrivateKeyPath)
		if kerr != nil {
			return nil, nil, "", kerr
		}
		githubApp, err = github.NewApp(gh.Issuer(), pem)
		if err != nil {
			return nil, nil, "", fmt.Errorf("github extension init failed: %w", err)
		}
		githubApp.SetPartialFixLabel(gh.Labels.PartialFix)
		extTools = githubApp.Tools()
		gitTokenSource = githubApp
		slog.Info("github extension enabled", "component", "startup", "issuer", gh.Issuer(), "mention", gh.Mention)
	}

	// runHub is needed by the SDK extensions built below (Dispatch fans a
	// run's events through it) as well as REST and the GitHub extension later.
	runHub := stream.NewHub()

	// orchRef is resolved after orch is built further down - an SDK
	// extension's Dispatch may not fire until long after construction, but
	// its Tools() are needed now to fold into extTools before buildAgents.
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	sdkExts, err := buildSDKExtensions(cfg, st, runHub, &orchRef, artifacts)
	if err != nil {
		return nil, nil, "", err
	}
	extTools = append(extTools, sdkExtensionTools(sdkExts)...)
	cleanups = append(cleanups, func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		stopSDKExtensions(stopCtx, sdkExts)
	})

	// Bring the plugin trees to their pinned refs before anything reads them,
	// and log what we actually got - skills change how every agent plans, so
	// the revision belongs in the startup record.
	pluginRevs := plugin.Refresh(pluginManifestPath, pluginFetchScript)
	if len(pluginRevs) > 0 {
		slog.Info("skill plugins resolved", "component", "startup", "revisions", plugin.Summary(pluginRevs))
	}

	builtinSkillSrc := newSkillSource(cfg.Skills.Plugins)
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
		cprov, ok := cfg.Provider(rm.Consolidation.Provider)
		if !ok {
			return nil, fmt.Errorf("consolidation provider %q not found", rm.Consolidation.Provider)
		}
		consolidator, err := inference.NewModel(cprov, rm.Consolidation.Model, artifacts)
		if err != nil {
			return nil, fmt.Errorf("consolidation model: %w", err)
		}
		return memory.New(context.Background(), rm.Kind, rm.URL, embedder, consolidator, rm.Collection, domain, rm.TopK, rm.MinScore)
	}
	var taskStore, userStore *memory.Store
	if rm, ok := cfg.MemoryStore("stage_memory"); ok {
		s, err := openMemory(rm, "task")
		if err != nil {
			return nil, nil, "", fmt.Errorf("task memory init failed: %w", err)
		}
		taskStore = s
		slog.Info("semantic memory enabled", "component", "startup", "collection", rm.Collection,
			"embedder", rm.Embedder.Model, "consolidation", rm.Consolidation.Model)
	}
	if slices.Contains(cfg.Orchestrator.Tools, "commit_memory") {
		if rm, ok := cfg.MemoryStore("commit_memory"); ok {
			s, err := openMemory(rm, "user")
			if err != nil {
				return nil, nil, "", fmt.Errorf("user memory init failed: %w", err)
			}
			userStore = s
			slog.Info("user memory enabled", "component", "startup", "collection", rm.Collection)
		}
	}

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

	var deliver vetting.DeliverFunc
	if githubApp != nil {
		deliver = githubApp.Deliver
	}
	var setupFn dag.SetupFunc
	clientMap, modelMap, nodeServers, judgeFactory, planJudge, gateCfgs, judgeModel, err := buildAgents(cfg, st.Sessions, skillTS, builtinSkillSrc, newScopedSkillTS, taskStore, advisorAgent, jail, gitTokenSource, extTools, deliver, nodeCancelled, &setupFn, artifacts)
	if err != nil {
		return nil, nil, "", fmt.Errorf("agent build failed: %w", err)
	}
	cleanups = append(cleanups, func() {
		nodeServers.closeAll()
	})

	agentInfos := make([]dag.AgentInfo, 0, len(clientMap))
	mediaAgents := make(map[string]bool)
	for name, c := range clientMap {
		agentInfos = append(agentInfos, dag.AgentInfo{Name: name, Description: c.Description()})
		ac := cfg.Agents[name]
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
	executorRef.Store(executor)
	orch := orchestrator.New(st.Sessions, llm, orchSysPrompt, planner, executor, orchSkillTS, userStore, taskStore)
	orch.SetMaxActiveRuns(cfg.Dag.MaxActiveRuns)
	if gh := cfg.Extensions.GitHub; gh != nil && gh.RunTimeoutMinutes > 0 {
		orch.SetRunDeadline(time.Duration(gh.RunTimeoutMinutes) * time.Minute)
	}
	orchRef.Store(orch)
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

	var extensions []extension.Extension
	if githubApp != nil {
		ghExt := github.NewExtension(githubApp, *cfg.Extensions.GitHub, orch, st, runHub)
		if judgeModel != nil {
			ghExt.SetIntentClassifier(&modelIntentClassifier{model: judgeModel})
		}
		ghExt.SetJail(jail, localUserID)
		extensions = append(extensions, ghExt)
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
		Extensions:    extensions,
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
func buildAgents(cfg *config.Config, sessions session.Service, skillTS *skilltoolset.SkillToolset, builtinSkillSrc skill.Source, newScopedSkillTS func(names []string) (*skilltoolset.SkillToolset, error), taskStore *memory.Store, advisorAgent adkagent.Agent, jail *workspace.Jail, gitTokenSource tools.GitTokenSource, extTools []tool.Tool, deliver vetting.DeliverFunc, nodeCancelled func(chatID, nodeID string) bool, setupOut *dag.SetupFunc, artifacts artifact.Service) (map[string]adkagent.Agent, map[string]model.LLM, *perNodeServers, vetting.JudgeFactory, vetting.PlanJudge, map[string]vetting.Config, model.LLM, error) {
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
			planJudge = vetting.NewPlanJudge(judge)
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
			_, err := tools.SetupClone(ctx, jail, localUserID, chatID, dir, setup.Repo, setup.BaseRef, setup.WorkBranch, setup.CheckoutExistingHead, workspaceCaps, gitCredentials, gitTokenSource)
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

	extToolsByName := make(map[string]tool.Tool, len(extTools))
	for _, t := range extTools {
		extToolsByName[t.Name()] = t
	}

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
			env := opencodeEnv(prov, ac, acpSkillPaths(cfg.Skills.Plugins))
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
				ExtraRO:         acpSkillPaths(cfg.Skills.Plugins),
				Home:            workspaceCaps.HomeDir,
				Preamble:        preamble,
				Jail:            jail,
				UserID:          localUserID,
				PermissionJudge: permJudge,
				Worktree: func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (string, error) {
					parentDir, err := jail.Resolve(userID, chatID, workspace.NodeDir(parentNodeID))
					if err != nil {
						return "", fmt.Errorf("acp worktree: resolve parent clone: %w", err)
					}
					return tools.SetupWorktree(ctx, jail, userID, chatID, parentDir, workspace.NodeDir(nodeID),
						workspace.WorktreeBranch(nodeID), workspaceCaps)
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
	if override, err := vetting.LoadBundleRubric(ac.Bundle); err != nil {
		return c, err
	} else if override != "" {
		c.Rubric = override
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
func opencodeEnv(prov config.ProviderConfig, ac config.AgentConfig, skillPaths []string) []string {
	type m = map[string]any
	apiKey := prov.APIKey
	if apiKey == "" {
		apiKey = "unused"
	}
	modelCfg := m{}
	if ac.ContextWindow > 0 {
		modelCfg["limit"] = m{"context": ac.ContextWindow, "output": 32768}
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
			"bash": m{
				"git push": "deny", "git push *": "deny",
				"git clone": "deny", "git clone *": "deny",
				"gh repo clone": "deny", "gh repo clone *": "deny",
				"*": "allow",
			},
			"external_directory": m{"*": "deny"},
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

// acpSkillPaths collects on-disk skill roots for an ACP agent's skills.paths:
// quack's own skills/, then each configured plugin's skills directory
// (internal/plugin discovery), in order.
func acpSkillPaths(pluginRoots []string) []string {
	var out []string
	if abs, err := filepath.Abs("skills"); err == nil {
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			out = append(out, abs)
		}
	}
	return append(out, plugin.ResolveSkillDirs(pluginRoots)...)
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
