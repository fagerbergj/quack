// Package serve is Quack's server bootstrap: it loads config, builds the
// inference model, orchestrator, and stores, and serves the REST + MCP API plus
// the embedded SPA. The `quack server run` command calls Run.
package serve

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/extension"
	"github.com/fagerbergj/quack/internal/github"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/server"
	mcpserver "github.com/fagerbergj/quack/internal/server/mcp"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// localUserID is the identity every filesystem/git tool's jail resolves
// against — Quack is single-user today (mirrors the same constant in
// internal/server/rest and internal/server/mcp); the OIDC subject replaces it
// the day multi-user lands, with no change to the jail's path resolution.
const localUserID = "local"

// vendorSkillsDir is the vendored ponytail skill library — a git submodule
// (github.com/DietrichGebert/ponytail, pinned by .gitmodules) whose skills/
// dir holds SKILL.md skills in exactly the layout the shipped skills/ library
// uses. Merged into the skill toolset as a second, lower-priority source when
// present on disk (newSkillSource); absent (submodule not initialized, or an
// installed binary outside the repo) the primary source alone serves — no
// error, the ponytail skills are just unavailable. Run
// `git submodule update --init` (or clone with --recursive) to populate it.
const vendorSkillsDir = ".agents/vendor/ponytail/skills"

// newSkillSource builds the skill toolset's Source: the shipped skills/
// library (disk-then-embedded via bundledir) plus, when vendorDir exists on
// disk, the vendored skills merged in behind it (primary wins on any name
// collision by mergedSource's query order; duplicate names across sources are
// a startup error, which is the right loudness for a vendoring mistake).
func newSkillSource(vendorDir string) skill.Source {
	primary := skill.NewFileSystemSource(bundledir.SubFS("skills"))
	if st, err := os.Stat(vendorDir); err != nil || !st.IsDir() {
		return primary
	}
	slog.Info("vendored skills merged into the skill library", "component", "startup", "dir", vendorDir)
	return skill.NewMergedSource(primary, skill.NewFileSystemSource(os.DirFS(vendorDir)))
}

//go:embed all:web/dist
var webDist embed.FS

// Run builds the server and serves it on cfg.Server.Addr (or :port) until ctx is
// cancelled (the caller wires SIGINT/SIGTERM). The standalone `quack server run`
// path; logs to stdout so a container/supervisor collects them.
func Run(ctx context.Context, configPath string, port int) error {
	setupLoggingTo(os.Stdout, slog.LevelInfo)
	handler, cleanup, addr, err := build(ctx, configPath, port)
	if err != nil {
		return err
	}
	defer cleanup()

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

// InProcess builds the server and serves it on an ephemeral loopback port,
// returning its base URL and a stop func. This is how `quack` runs the duck
// locally — co-hosted in the CLI process, no separate `quack server run`. Logs go
// to stderr (default warn) so the client's stdout stays clean (e.g. `quack -p`).
func InProcess(ctx context.Context, configPath string) (baseURL string, stop func() error, err error) {
	setupLoggingTo(os.Stderr, slog.LevelWarn)
	handler, cleanup, _, err := build(ctx, configPath, 0)
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

// build loads config and constructs the HTTP handler (orchestrator + stores +
// agents + SPA), shared by Run and InProcess. It returns the handler, a cleanup
// func (close A2A servers; note managed stores), and the listen addr. On any
// error it runs the cleanups registered so far and returns the error.
func build(ctx context.Context, configPath string, port int) (handler http.Handler, cleanup func(), addr string, err error) {
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

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("config load failed: %w", err)
	}
	addr = cfg.Server.Addr
	if port != 0 {
		addr = fmt.Sprintf(":%d", port)
	}

	// Workspace (filesystem/git tools' isolation boundary): one jail per server
	// process, rooted at workspace.root (config.Load already defaulted it to
	// ./workspace). Built unconditionally — cheap, and it means wiring is ready
	// the moment an agent's tools: list requests read_file etc. (not yet done by
	// any shipped agent — see .quack/plan-pr5-tool-schemas.md).
	jail, err := workspace.NewJail(cfg.Workspace.Root)
	if err != nil {
		return nil, nil, "", fmt.Errorf("workspace init failed: %w", err)
	}

	// Managed topology: bring up the Postgres + Qdrant stores via docker compose
	// before opening them. embedded/external just run against pre-configured
	// stores. Stores are left running on exit (persistent infra — restart the app
	// freely); tear them down with `docker compose -p quack-stores down`.
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
	if n, err := st.FailStaleDagNodes(context.Background()); err != nil {
		slog.Error("fail stale dag nodes", "component", "store", "err", err)
	} else if n > 0 {
		slog.Info("marked orphaned dag nodes failed (previous process killed mid-run)", "component", "store", "count", n)
	}

	prov, _ := cfg.Provider(cfg.Orchestrator.Provider)
	llm, err := inference.NewModel(prov, cfg.Orchestrator.Model)
	if err != nil {
		return nil, nil, "", fmt.Errorf("inference model init failed: %w", err)
	}

	// GitHub extension (off unless extensions.github is configured). Built here,
	// BEFORE the agents, so its App can serve as the dynamic git-credential
	// source and its Tools() join every agent's tool set. The webhook Runner is
	// bound after the orchestrator exists (below).
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
		extTools = githubApp.Tools()
		gitTokenSource = githubApp // App implements tools.GitTokenSource
		slog.Info("github extension enabled", "component", "startup", "issuer", gh.Issuer(), "mention", gh.Mention)
	}

	// Load skills once at startup; pass the toolset to every specialist agent so
	// all agents can call load_skill / list_skills / load_skill_resource. Skills
	// resolve from disk in cwd first (live repo edits) then the embedded copy,
	// so an installed binary works from any directory; the vendored ponytail
	// library (a git submodule — see vendorSkillsDir) merges in when present.
	// Project-aware: on top of the built-in library (shipped + vendored), also
	// surface a cloned repo's own .agents/skills / .claude/skills at load_skill
	// time (discovered in the jail per query). Built-in ALWAYS wins a name
	// collision — a cloned repo can't hijack a core skill; project skills are
	// purely additive (see internal/skillsource).
	skillSrc := skillsource.New(newSkillSource(vendorSkillsDir), jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		return nil, nil, "", fmt.Errorf("skills toolset init failed: %w", err)
	}

	// Semantic memory (M6): a memory tool bound to a vector store (with QUACK_QDRANT_URL
	// set) turns it on — config composes it, no dedicated block. Task memory
	// follows `stage_memory` (researchers' recall + the trust gate's vetted
	// commit); user memory follows `commit_memory` bound to the orchestrator. A
	// store with no URL self-disables, so qdrant-less runs keep working.
	openMemory := func(rm config.ResolvedMemory, domain string) (*memory.Store, error) {
		eprov, ok := cfg.Provider(rm.Embedder.Provider)
		if !ok {
			return nil, fmt.Errorf("embedder provider %q not found", rm.Embedder.Provider)
		}
		embedder, err := inference.NewEmbedder(eprov, rm.Embedder.Model)
		if err != nil {
			return nil, fmt.Errorf("embedder: %w", err)
		}
		cprov, ok := cfg.Provider(rm.Consolidation.Provider)
		if !ok {
			return nil, fmt.Errorf("consolidation provider %q not found", rm.Consolidation.Provider)
		}
		consolidator, err := inference.NewModel(cprov, rm.Consolidation.Model)
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
	// User memory: presence of commit_memory on the orchestrator is the switch.
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

	// Advisor: the worker's ask_advisor mentor tool, built once here (judge's
	// model + the agents/advisor bundle, tool-less) so buildAgents can wire it
	// into worker bundles that list ask_advisor. nil (tool absent) when gating
	// is off or the build fails — never fails startup. Not added to the
	// plannable roster — it's a tool, not a DAG node.
	var advisorAgent adkagent.Agent
	if cfg.Gates.JudgeEnabled() {
		if aprov, ok := cfg.Provider(cfg.Gates.Judge.Provider); ok {
			if am, merr := inference.NewModel(aprov, cfg.Gates.Judge.Model); merr != nil {
				slog.Warn("advisor model build failed; ask_advisor disabled", "component", "startup", "err", merr)
			} else if ab, berr := agent.LoadBundle("agents/advisor"); berr != nil {
				slog.Warn("advisor bundle load failed; ask_advisor disabled", "component", "startup", "err", berr)
			} else if built, aerr := agent.BuildChat(ab, am, nil, nil, agent.Compaction{}, ""); aerr != nil {
				slog.Warn("advisor build failed; ask_advisor disabled", "component", "startup", "err", aerr)
			} else {
				advisorAgent = built
				slog.Info("advisor enabled", "component", "startup", "model", cfg.Gates.Judge.Model)
			}
		}
	}

	// The DAG executor is built below (it needs the agents this call produces),
	// but the workers' TOOLS need to ask it "was my node cancelled?" on every
	// call — so hand buildAgents a predicate that reads the executor through a
	// holder, published once startup has built it. Nothing calls a tool before
	// the server is listening, and the atomic keeps the publish race-free.
	var executorRef atomic.Pointer[dag.Executor]
	nodeCancelled := func(chatID, nodeID string) bool {
		ex := executorRef.Load()
		return ex != nil && ex.NodeCancelled(chatID, nodeID)
	}
	// Same holder, for steer's tool-layer half (steerguard.go): a worker's next
	// tool call gets pending guidance as that call's result instead of running.
	nodeSteerGuidance := func(chatID, nodeID string) string {
		ex := executorRef.Load()
		if ex == nil {
			return ""
		}
		return ex.NodeSteerGuidance(chatID, nodeID)
	}

	// Build each declarative agent, expose it over A2A, and collect a client the
	// DAG executor can dispatch to. Servers run for the process lifetime.
	clientMap, modelMap, servers, judgeFactory, gateCfgs, err := buildAgents(cfg, st.Sessions, skillTS, taskStore, advisorAgent, jail, gitTokenSource, extTools, nodeCancelled, nodeSteerGuidance)
	if err != nil {
		return nil, nil, "", fmt.Errorf("agent build failed: %w", err)
	}
	cleanups = append(cleanups, func() {
		for _, s := range servers {
			_ = s.Close()
		}
	})

	// Build agent info list for the planner (name + description) and a set of
	// media-capable agents (those with "image" or "audio" in their Inputs config)
	// so the executor knows which nodes should receive attachment parts.
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
	// Sort for a stable prompt (map iteration order is random) and render the
	// roster the orchestrator authors its DAG over.
	sort.Slice(agentInfos, func(i, j int) bool { return agentInfos[i].Name < agentInfos[j].Name })
	var rosterSB strings.Builder
	for _, a := range agentInfos {
		fmt.Fprintf(&rosterSB, "- `%s` — %s\n", a.Name, a.Description)
	}

	// Load orchestrator bundle for its system prompt.
	orchBundle, err := agent.LoadBundle("agents/orchestrator")
	if err != nil {
		return nil, nil, "", fmt.Errorf("orchestrator bundle load failed: %w", err)
	}
	// Load the format-markdown skill frontmatter so the orchestrator's prompt
	// lists it and the model knows to call load_skill("format-markdown") for
	// direct-answer responses.
	fmFm, err := skillSrc.LoadFrontmatter(context.Background(), "format-markdown")
	if err != nil {
		return nil, nil, "", fmt.Errorf("format-markdown skill load failed: %w", err)
	}
	// plan-work is the DAG-authoring playbook the orchestrator loads before planning.
	planWorkFm, err := skillSrc.LoadFrontmatter(context.Background(), "plan-work")
	if err != nil {
		return nil, nil, "", fmt.Errorf("plan-work skill load failed: %w", err)
	}
	// When user memory is on, append the orchestrator's memory.md guidance (its
	// "what to remember about the user" section) to its behaviour — gated the same
	// way agent bundles are.
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

	planner := dag.NewPlanner(agentInfos, cfg.Workspace.CheckCommands)
	// cfgFor supplies the per-agent trust-gate config to the graph executor; a
	// non-gated (or gates-disabled) agent gets the zero Config (JudgeRounds=0), so
	// RunGatedRefine runs the worker once and returns it ungated.
	cfgFor := func(name string) vetting.Config { return gateCfgs[name] }
	executor := dag.NewExecutor(st.Sessions, clientMap, modelMap, judgeFactory, cfgFor, mediaAgents)
	executor.SetMaxActive(cfg.Dag.MaxActiveNodes)
	executorRef.Store(executor) // arms the tools' cancel guard (see nodeCancelled above)
	orch := orchestrator.New(st.Sessions, llm, orchSysPrompt, planner, executor, skillTS, userStore)

	spa, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, nil, "", fmt.Errorf("embed SPA fs failed: %w", err)
	}

	// Now the orchestrator exists, bind it as the extension's webhook Runner and
	// mount the extension's inbound routes.
	var extensions []extension.Extension
	if githubApp != nil {
		extensions = append(extensions, github.NewExtension(githubApp, *cfg.Extensions.GitHub, orch))
	}

	handler = server.New(server.Options{
		REST:       rest.NewHandler(st, orch, llm, jail),
		MCP:        mcpserver.Handler(orch),
		SPA:        spa,
		Extensions: extensions,
	})
	return handler, runCleanups, addr, nil
}

// setupLoggingTo installs the process-wide slog handler writing to w, at
// QUACK_LOG_LEVEL (debug|info|warn|error) or `fallback` when that env is unset,
// in text or QUACK_LOG_FORMAT=json. SetDefault also reroutes stray stdlib log.*.
// `quack server run` logs to stdout (a supervisor collects them); the in-process
// duck logs to stderr at warn so the client's stdout stays clean.
func setupLoggingTo(w io.Writer, fallback slog.Level) {
	lvl := fallback
	if s := os.Getenv("QUACK_LOG_LEVEL"); s != "" {
		_ = lvl.UnmarshalText([]byte(s)) // unknown values leave fallback untouched
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if strings.EqualFold(os.Getenv("QUACK_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
}

// buildAgents loads each configured agent bundle, builds its model and built-in
// tools, exposes it over a co-located A2A server, and returns:
//   - clientMap: agent name → A2A client (for the DAG executor)
//   - servers: A2A server handles (to close on shutdown)
func buildAgents(cfg *config.Config, sessions session.Service, skillTS *skilltoolset.SkillToolset, taskStore *memory.Store, advisorAgent adkagent.Agent, jail *workspace.Jail, gitTokenSource tools.GitTokenSource, extTools []tool.Tool, nodeCancelled func(chatID, nodeID string) bool, nodeSteerGuidance func(chatID, nodeID string) string) (map[string]adkagent.Agent, map[string]model.LLM, []*agent.A2AServer, vetting.JudgeFactory, map[string]vetting.Config, error) {
	// nodeScope resolves the part of an agent's memory entitlement that is only
	// knowable per invocation: the repo the node is working in, and the real user.
	// Neither survives the A2A hop on its own (a worker's ctx.UserID() is the
	// per-invocation "A2A_USER_<ctxid>"), so we reuse the ONE channel that does —
	// the advisor-thread marker the gate stamps into the worker's prompt (the same
	// channel guard.go and the workspace tools' chat scope use) — and derive the repo
	// from the chat's jail (the single clone in it; "" when there is none or several,
	// so memory falls back to the role bucket rather than guessing).
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

	// Filesystem tool caps, converted once from config (defaults already
	// applied by config.Load's validation).
	workspaceCaps := workspace.Caps{
		MaxReadBytes:   int64(cfg.Workspace.MaxReadKB) * 1024,
		MaxWriteBytes:  int64(cfg.Workspace.MaxWriteKB) * 1024,
		MaxResults:     cfg.Workspace.MaxResults,
		MaxListEntries: cfg.Workspace.MaxListEntries,
		Timeout:        time.Duration(cfg.Workspace.TimeoutSeconds) * time.Second,
		ExtraPath:      cfg.Workspace.ExecPath,
		Limits: workspace.Limits{
			AddressSpaceMB: cfg.Workspace.Limits.AddressSpaceMB,
			Procs:          cfg.Workspace.Limits.MaxProcs,
			FileSizeMB:     cfg.Workspace.Limits.MaxFileSizeMB,
		},
	}
	// The OS boundary every run_command / gate-check child process runs inside.
	// Resolved HERE, once, at startup — and it either holds or the server does
	// not start: a configured-but-unusable sandbox is a hard error (the jail is
	// a path check on the TOOLS; without this, a child — including any `sh -c` —
	// had the server user's whole filesystem). `none` returns cleanly, with a
	// WARN that says exactly what the deployment is accepting.
	sandbox, err := workspace.ResolveSandbox(workspace.SandboxMode(cfg.Workspace.Sandbox))
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	workspaceCaps.Sandbox = sandbox
	// A dedicated $HOME for run_command/checks/git children, OUTSIDE the
	// user's cloned repos (a sibling under their jail root — see Jail.
	// HomeDir) — never the task's own cwd. Fixes a live bug: HOME pinned to a
	// coding task's cwd (the target repo itself) meant `npm ci` wrote its
	// cache directly into the repo tree, and git_commit's add_all then swept
	// the cache up alongside the real change (1,261 garbage files in one
	// commit). Wired onto workspaceCaps so every consumer (fs/git/run_command
	// tools AND the trust gate's deterministic checks) gets it for free.
	homeDir, err := jail.HomeDir(localUserID)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("workspace home dir init failed: %w", err)
	}
	workspaceCaps.HomeDir = homeDir

	var judgeFactory vetting.JudgeFactory
	var gateCfg vetting.Config
	// safetyJudge backs the guard ladder's judge tier (internal/tools/guard.go):
	// an independent single-shot allow/deny call reusing the SAME judge
	// model/provider as the trust gate's judge stage (gates.judge) — see the
	// design doc §4b. nil when the judge stage is off; a tool configured for a
	// judge tier then fails closed at call time (guardedTool.Run), not silently.
	var safetyJudge tools.SafetyJudge
	if cfg.Gates.Enabled() {
		var err error
		if gateCfg, err = vetting.FromConfig(cfg.Gates); err != nil {
			return nil, nil, nil, nil, nil, err
		}
		// The trust gate commits vetted tradecraft on a judge pass (M6). nil
		// *memory.Store when memory is off — the gate's nil check handles it.
		gateCfg.Memory = taskStore
		// §4's per-node deterministic checks execute through the SAME jail,
		// identity, and caps every fs/git/run_command tool call already uses —
		// wired onto the BASE Config here so every gated agent's per-node copy
		// (dag.buildGateNodes) inherits it; only Checks/Workdir vary per node.
		gateCfg.Workspace = jail
		gateCfg.WorkspaceUserID = localUserID
		gateCfg.WorkspaceCaps = workspaceCaps
		// The allowlist every check must prefix-match — the security boundary for
		// BOTH planner-set checks (validated at plan time) and the checks the gate
		// derives from the repo (vetting.deriveChecks). Empty ⇒ checks disabled.
		gateCfg.CheckCommands = cfg.Workspace.CheckCommands
		// The judge model is only built when the judge stage is active; the
		// deterministic + self-critique stages run without it. Citation backing is
		// checked deterministically in code, so the judge no longer carries web
		// tools (a re-fetch loop is wasted work for ~no gain). It IS given the
		// four read-only workspace tools when a jail is configured, so a coding
		// node's judge can OPEN the files the worker wrote/changed and score code
		// quality from the real source instead of blindly trusting the answer's
		// self-report. Read-only by construction — never write_file/edit_file/
		// delete_path/git_*/run_command; the judge must not mutate or run anything.
		if cfg.Gates.JudgeEnabled() {
			jprov, ok := cfg.Provider(cfg.Gates.Judge.Provider)
			if !ok {
				return nil, nil, nil, nil, nil, fmt.Errorf("gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
			}
			judge, err := inference.NewModel(jprov, cfg.Gates.Judge.Model)
			if err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("gates.judge: model: %w", err)
			}
			var judgeReadTools []tool.Tool
			if jail != nil {
				judgeReadTools, err = tools.Build([]string{"read_file", "list_dir", "glob", "grep"}, tools.Deps{
					Workspace:       jail,
					WorkspaceUserID: localUserID,
					WorkspaceCaps:   workspaceCaps,
				})
				if err != nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("gates.judge: read tools: %w", err)
				}
			}
			// The judge also gets the SAME skill toolset the workers hold, so it
			// can agentically load a relevant review/quality skill (e.g.
			// ponytail-review) and ground its judgment in the same principles the
			// worker followed, rather than those principles being statically baked
			// into the judge prompt. Skill lookups are read-only content fetches,
			// safe in the judge's isolated runner.
			var judgeSkillsets []tool.Toolset
			if skillTS != nil {
				judgeSkillsets = []tool.Toolset{skillTS}
			}
			judgeFactory = vetting.NewJudgeFactory(judge, judgeReadTools, judgeSkillsets)
			safetyJudge = tools.NewSafetyJudge(judge)
		}
		slog.Info("trust gate enabled", "component", "startup",
			"deterministic_rounds", gateCfg.DeterministicRounds,
			"judge", cfg.Gates.Judge.Model, "judge_rounds", gateCfg.JudgeRounds, "threshold", gateCfg.Threshold)
	}

	// Git tools' deployment-level credentials + push switch (workspace.*, §4b/
	// "Git auth" of the design doc). GitCredentials is empty when unconfigured
	// (public repos only); config.Load already enforced token: ${VAR}-only.
	gitCredentials := make([]tools.GitCredential, len(cfg.Workspace.GitCredentials))
	for i, gc := range cfg.Workspace.GitCredentials {
		gitCredentials[i] = tools.GitCredential{Host: gc.Host, Username: gc.Username, Token: gc.Token}
	}

	// Build the compaction summariser once and share it across every gated agent.
	// An agent with no context_window configured is left uncompacted; see
	// compactionFor below.
	var summarizer model.LLM
	compCfg := cfg.Session.Compaction
	if compCfg.Enabled {
		cprov, ok := cfg.Provider(compCfg.Provider)
		if !ok {
			return nil, nil, nil, nil, nil, fmt.Errorf("compaction: provider %q not found", compCfg.Provider)
		}
		var err error
		if summarizer, err = inference.NewModel(cprov, compCfg.Model); err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("compaction: model: %w", err)
		}
		slog.Info("context compaction enabled", "component", "startup", "summariser", compCfg.Model)
	}
	compactionFor := func(ac config.AgentConfig) agent.Compaction {
		if !compCfg.Enabled {
			return agent.Compaction{}
		}
		if ac.ContextWindow == 0 {
			slog.Warn("context compaction: agent has no context_window configured; not compacting it", "component", "startup", "model", ac.Model)
			return agent.Compaction{}
		}
		return agent.Compaction{
			Summarizer:    summarizer,
			ContextWindow: ac.ContextWindow,
			Enabled:       true,
		}
	}

	clientMap := make(map[string]adkagent.Agent, len(cfg.Agents))
	modelMap := make(map[string]model.LLM, len(cfg.Agents))
	gateCfgs := make(map[string]vetting.Config, len(cfg.Agents)) // agent name → per-agent gate cfg (gated agents only)
	var servers []*agent.A2AServer

	for _, name := range names {
		ac := cfg.Agents[name]

		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			return nil, nil, servers, nil, nil, fmtErr(name, "provider %q not found", ac.Provider)
		}
		m, err := inference.NewModel(prov, ac.Model)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "model: %v", err)
		}

		// Memory tools (M6) are ADK-native and route through the runner's
		// MemoryService (set in agent.Serve). preload_memory is ambient recall
		// (added to every agent when memory is on); load_memory is deliberate recall
		// (opt-in via the agent's tools list). Strip load_memory from the builtin
		// names regardless, so tools.Build never sees an unknown tool.
		toolNames, wantLoadMemory := resolveToolNames(ac.Tools, taskStore != nil, advisorAgent != nil)
		var builtins []tool.Tool
		if len(toolNames) > 0 {
			builtins, err = tools.Build(toolNames, tools.Deps{
				WebSearch:         tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL, Key: cfg.Tools["web_search"].APIKey()},
				Fetch:             tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
				Summarizer:        m,
				Cache:             urlCache,
				Advisor:           advisorAgent,
				Sessions:          sessions,
				Workspace:         jail,
				WorkspaceUserID:   localUserID,
				WorkspaceCaps:     workspaceCaps,
				GitCredentials:    gitCredentials,
				GitTokenSource:    gitTokenSource,
				GitPush:           cfg.Workspace.GitPush,
				Guards:            cfg.Workspace.Guards,
				SafetyJudge:       safetyJudge,
				NodeCancelled:     nodeCancelled,
				NodeSteerGuidance: nodeSteerGuidance,
			})
			if err != nil {
				return nil, nil, servers, nil, nil, fmtErr(name, "tools: %v", err)
			}
		}
		// Extension outbound tools (e.g. github_comment / github_pull_request)
		// join every agent's tool set, alongside the builtins and skill toolset.
		builtins = append(builtins, extTools...)

		bundle, err := agent.LoadBundle(ac.Bundle)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "bundle: %v", err)
		}
		// Memory guidance (M6): the bundle's optional memory.md. Its presence marks the
		// agent as a memory participant — ONLY such agents get recall (preload/load_memory).
		// Tool-less combiners (synthesizer) and media/image readers have no memory.md, so
		// they never touch the embedder. That matters: the synthesizer's input is all
		// upstream findings concatenated (tens of KB), and embedding that on the CPU
		// embedder is a large, useless job that head-of-line-blocks the queue and stalls
		// the whole DAG. Recall belongs only to agents that research.
		var memGuidance string
		if taskStore != nil {
			if memGuidance, err = agent.LoadBundleMemory(ac.Bundle); err != nil {
				return nil, nil, servers, nil, nil, fmtErr(name, "memory.md: %v", err)
			}
		}
		// The agent's recall service: a VIEW of the shared store bound to this agent's
		// buckets — its role family + the per-node repo/user (nodeScope) — plus its own
		// agent name as the legacy read key, so memories written under the old
		// per-agent-silo scheme still load. nil (not a typed-nil interface) when this
		// agent is not a memory participant.
		var memSvc adkmemory.Service
		if taskStore != nil && memGuidance != "" {
			memSvc = taskStore.View(memory.Scope{Role: ac.MemoryRole, Legacy: name}, nodeScope)
			builtins = append(builtins, memory.NewPreload())
			if wantLoadMemory {
				builtins = append(builtins, loadmemorytool.New())
			}
		}
		comp := compactionFor(ac)
		ag, err := agent.Build(bundle, m, builtins, []tool.Toolset{skillTS}, comp, memGuidance)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "build: %v", err)
		}

		// The agent is served PLAIN — the trust gate is applied per-node by the graph
		// (dag.BuildWorkflow → vetting.RunGatedRefine), not wrapped around the agent.
		// Here we only collect the per-agent gate config (base + this bundle's rubric
		// override + memory participation) for the executor's cfgFor.
		if cfg.Gates.Enabled() && !ac.IsGated() {
			slog.Info("trust gate skipped for agent (gated: false)", "component", "startup", "agent", name)
		}
		if cfg.Gates.Enabled() && ac.IsGated() {
			agentGateCfg := gateCfg
			// An agent participates in task memory iff it has a memory.md (loaded as
			// memGuidance above). Such agents commit on a judge pass even when they
			// staged nothing, so Commit's answer-extraction still runs.
			agentGateCfg.CommitMemory = taskStore != nil && memGuidance != ""
			// The role bucket this agent reads and writes (memory is shared, bucketed by
			// subject — see internal/memory/scope.go).
			agentGateCfg.MemoryRole = ac.MemoryRole
			// A retrieval agent (web tools in its list) must actually retrieve —
			// a zero-activity answer hard-fails the deterministic fold instead of
			// sailing to the judge ungraded (see vetting.Config.RequireRetrieval).
			for _, tn := range ac.Tools {
				if tn == "web_search" || tn == "web_fetch" {
					agentGateCfg.RequireRetrieval = true
					break
				}
			}
			if override, err := vetting.LoadBundleRubric(ac.Bundle); err != nil {
				return nil, nil, servers, nil, nil, fmtErr(name, "rubric: %v", err)
			} else if override != "" {
				agentGateCfg.Rubric = override
				slog.Info("using per-agent rubric from bundle", "component", "startup", "agent", name)
			}
			// Per-agent judge/revise round budget (0 ⇒ inherit the global default).
			// The economics differ by agent: research converges in one round (extra
			// rounds burn tokens re-fetching), whereas coding genuinely needs the
			// judge+revise grind to iterate until tests pass.
			if ac.JudgeRounds > 0 {
				agentGateCfg.JudgeRounds = ac.JudgeRounds
			}
			// judge: false forces JudgeRounds to 0, so the gate skips the independent
			// judge entirely (RunGatedRefine's round loop is round <= JudgeRounds). Used
			// where the judge model cannot evaluate the output at all — a text judge
			// scoring a media transcription it never saw is noise, not a check.
			if ac.Judge != nil && !*ac.Judge {
				agentGateCfg.JudgeRounds = 0
			}
			slog.Info("per-agent trust gate config", "component", "startup", "agent", name, "judge_rounds", agentGateCfg.JudgeRounds)
			gateCfgs[name] = agentGateCfg
		}

		srv, err := agent.Serve(ag, sessions, memSvc)
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "a2a serve: %v", err)
		}
		servers = append(servers, srv)

		client, err := srv.Client()
		if err != nil {
			return nil, nil, servers, nil, nil, fmtErr(name, "a2a client: %v", err)
		}
		clientMap[name] = client
		modelMap[name] = m
		// tools listed here is the definitive record of what the agent can call —
		// when a worker "didn't use its tool", check this line first.
		slog.Info("agent serving over A2A", "component", "startup", "agent", name, "url", srv.Card.SupportedInterfaces[0].URL, "tools", ac.Tools)
	}
	return clientMap, modelMap, servers, judgeFactory, gateCfgs, nil
}

// contentText flattens a content's text parts (the worker's prompt, where the gate
// stamps the advisor-thread marker nodeScope reads).
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

// resolveToolNames splits an agent bundle's configured tool names into the
// builtin names tools.Build should actually construct, plus whether
// load_memory (ADK-native, added separately by the caller) was requested.
// Two names are gated on runtime availability rather than erroring when their
// dependency is off, so one config file describes every topology:
//   - stage_memory needs a task-memory store (taskMemAvailable).
//   - ask_advisor needs the advisor agent to consult (advisorAvailable) —
//     built only when gates.judge is enabled (see build's advisorAgent).
func resolveToolNames(configured []string, taskMemAvailable, advisorAvailable bool) (names []string, wantLoadMemory bool) {
	names = make([]string, 0, len(configured))
	for _, t := range configured {
		switch t {
		case "load_memory":
			wantLoadMemory = true // ADK-native; added below when memory is on
			continue
		case "stage_memory":
			if !taskMemAvailable {
				continue // memory off: don't build a sink that never commits
			}
		case "ask_advisor":
			if !advisorAvailable {
				continue // advisor disabled (judge off or advisor build failed): no mentor to consult
			}
		}
		names = append(names, t)
	}
	return names, wantLoadMemory
}
