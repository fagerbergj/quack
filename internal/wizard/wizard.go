// Package wizard is the interactive `quack init` / `quack server init` surface: thin Huh forms over
// internal/cli, which holds the logic so tests check emitted YAML, not keystrokes.
package wizard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/fagerbergj/quack/internal/cli"
)

// ErrAborted is returned when the user cancels the wizard at the existing-config
// gate. Callers treat it as a clean stop (no error printed, nothing written).
var ErrAborted = errors.New("init cancelled")

// ServerInit runs the server-config wizard, writing quack.yaml at outPath; an existing file without --force
// is offered keep/overwrite/elsewhere first. local narrows stores to one confirm.
func ServerInit(ctx context.Context, outPath string, force, local bool) (wrote bool, err error) {
	if !force && fileExists(outPath) {
		switch askExisting(outPath) {
		case existingUse:
			fmt.Printf("Using existing %s - no changes.\n", outPath)
			return false, nil
		case existingNewPath:
			p := askPath(outPath)
			if p == "" {
				return false, ErrAborted
			}
			outPath = p
		case existingOverwrite:
			// fall through: run the wizard and overwrite outPath.
		default: // cancel or escape
			fmt.Println("Cancelled - nothing changed.")
			return false, ErrAborted
		}
	}

	a := cli.InitAnswers{
		WebSearch: true, // optional features default on; the user can turn them off
		WebFetch:  true,
	}
	cli.PrefillFromEnv(&a) // don't re-ask what the environment already answers

	// Form 1 stands alone because its endpoint's model list must be fetched before
	// the rest of the wizard can offer it as choices.
	if err := askProvider(ctx, &a); err != nil {
		return false, err
	}
	models, manual := discoverModels(ctx, &a)

	// Form 2 is everything else as ONE multi-group form, so shift+tab walks back
	// across models → features → stores → review without hitting a wall.
	feats := featureList(&a)
	var ok bool
	groups := modelGroups(&a, models, manual)
	groups = append(groups, featuresGroup(&feats))
	groups = append(groups, codingGroups(&a, &feats, models)...)
	groups = append(groups, storeGroups(&a, &feats, local)...)
	groups = append(groups, reviewGroup(&a, &feats, outPath, &ok))
	if err := runForm(huh.NewForm(groups...)); err != nil {
		return false, err
	}
	a.WebSearch = slices.Contains(feats, "search")
	a.WebFetch = slices.Contains(feats, "fetch")
	a.Coding = slices.Contains(feats, "coding")
	if !ok {
		fmt.Println("Aborted - nothing written.")
		return false, nil
	}

	if err := cli.WriteServerConfig(a, outPath); err != nil {
		return false, err
	}
	return true, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Existing-config actions for the up-front gate when quack.yaml already exists.
const (
	existingUse       = "use"
	existingOverwrite = "overwrite"
	existingNewPath   = "newpath"
	existingCancel    = "cancel"
)

// askExisting asks what to do about an existing config before running the wizard.
// A form error/escape is treated as cancel.
func askExisting(outPath string) string {
	choice := existingCancel
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Options(
				huh.NewOption("Use it as-is", existingUse),
				huh.NewOption("Reconfigure - overwrite "+outPath, existingOverwrite),
				huh.NewOption("Write a new config to a different path", existingNewPath),
				huh.NewOption("Cancel", existingCancel),
			).
			Value(&choice),
	).Title(outPath + " already exists").Description("What would you like to do?"))
	if err := runForm(form); err != nil {
		return existingCancel
	}
	return choice
}

// askPath prompts for an alternate config path (prefilled with the current one).
// Returns "" on cancel/escape or an empty entry.
func askPath(cur string) string {
	p := cur
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Config path").Value(&p),
	).Title("New config path").Description("Where to write the new quack.yaml"))
	if err := runForm(form); err != nil {
		return ""
	}
	return strings.TrimSpace(p)
}

// runForm: duck theme on a stable alt-screen (inline rendering scrolls a tall group's title away).
// WithProgramOptions replaces huh's defaults, so stderr output (keeping stdout pipeable) and focus are re-supplied.
func runForm(f *huh.Form) error {
	return f.WithTheme(duckTheme()).
		WithProgramOptions(
			tea.WithOutput(os.Stderr),
			tea.WithReportFocus(),
			tea.WithAltScreen(),
		).Run()
}

// reviewGroup: a live summary note plus confirm in one group; the note recomputes via DescriptionFunc
// so backing up updates it, and ok stays false unless the user confirms.
func reviewGroup(a *cli.InitAnswers, feats *[]string, outPath string, ok *bool) *huh.Group {
	return huh.NewGroup(
		huh.NewNote().DescriptionFunc(func() string { return summarize(a, feats) }, a),
		huh.NewConfirm().Title("Write "+outPath+"?").Value(ok),
	).Title("Review").Description("Confirm before writing " + outPath)
}

func summarize(a *cli.InitAnswers, feats *[]string) string {
	feat := "none"
	if len(*feats) > 0 {
		feat = strings.Join(*feats, ", ")
	}
	s := fmt.Sprintf(
		"endpoint   %s\nmain       %s\njudge      %s\nembed      %s\nsession    %s\nfeatures   %s",
		a.Endpoint, a.MainModel, noneLabel(a.JudgeModel), noneLabel(a.EmbedModel), a.SessionKind, feat,
	)
	if slices.Contains(*feats, "coding") {
		coder := a.CoderModel
		if coder == "" {
			coder = a.MainModel + " (main)"
		}
		s += fmt.Sprintf("\ncoder      %s\nsandbox    %s", coder, a.Sandbox)
	}
	return s
}

func noneLabel(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// ClientInit is `quack init`: local (run quack here) or remote (connect to one).
// Local runs ServerInit then registers localhost; remote just registers.
func ClientInit(ctx context.Context, serverInitPath string, force bool) error {
	var mode string
	if err := runForm(huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("How will you use quack?").
			Options(
				huh.NewOption("Local  - run quack on this machine", "local"),
				huh.NewOption("Remote - connect to a server someone else runs", "remote"),
			).
			Value(&mode),
	).Title("Welcome"))); err != nil {
		return err
	}

	switch mode {
	case "local":
		// Local registers nothing: with no active server the CLI runs the duck in-process,
		// so writing quack.yaml is the whole job.
		wrote, err := ServerInit(ctx, serverInitPath, force, true)
		if err != nil {
			if errors.Is(err, ErrAborted) {
				return nil // user cancelled at the existing-config gate
			}
			return err
		}
		// Drop a legacy `local → localhost:8080` entry (and its active flag): local runs in-process,
		// so resolution would otherwise dial a server that isn't running.
		if c, err := cli.LoadClient(); err == nil {
			if _, ok := c.Servers["local"]; ok {
				c.RemoveServer("local")
				_ = c.Save()
			}
		}
		if wrote {
			fmt.Println("\nRun `quack -p \"<your question>\"` to chat now (in-process), or `quack server run` for the web UI on http://localhost:8080.")
		}
		return nil
	case "remote":
		return registerRemote()
	default:
		return nil
	}
}

func registerRemote() error {
	var name, url string
	if err := runForm(huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Server name").Placeholder("prod").Value(&name),
		huh.NewInput().Title("Server URL").Placeholder("https://quack.example.com").Value(&url),
	).Title("Remote server"))); err != nil {
		return err
	}
	c, err := cli.LoadClient()
	if err != nil {
		return err
	}
	if err := c.AddServer(name, url); err != nil {
		return err
	}
	if err := c.Use(name); err != nil {
		return err
	}
	if err := c.Save(); err != nil {
		return err
	}
	fmt.Printf("✓ Registered %s (%s) as your active server.\n", name, url)
	return nil
}

// askProvider: provider, endpoint and API key in one group so they navigate together;
// the key is masked.
func askProvider(ctx context.Context, a *cli.InitAnswers) error {
	var kind string
	return runForm(huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("LLM provider").
			Options(huh.NewOption("OpenAI-compatible", "openai")).
			Value(&kind),
		huh.NewInput().
			Title("Endpoint").
			Placeholder("http://localhost:11436/v1").
			Value(&a.Endpoint),
		huh.NewInput().
			Title("API key").
			Placeholder("blank if none").
			EchoMode(huh.EchoModePassword).
			Value(&a.APIKey),
	).Title("LLM provider").Description("How quack reaches its model server")))
}

// discoverModels calls /models and pre-selects by heuristic so the common case is confirm-confirm-confirm.
// manual is true when /models is unreachable (text inputs per role instead).
func discoverModels(ctx context.Context, a *cli.InitAnswers) (models []string, manual bool) {
	models, err := cli.ListModels(ctx, a.Endpoint, a.APIKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "couldn't reach %s/models (%v) - entering model names manually.\n", a.Endpoint, err)
		return nil, true
	}

	if a.MainModel == "" {
		a.MainModel = suggestMain(models)
	}
	// Pre-select specialist roles by model name (overridable; None disables):
	// /models returns IDs only, with no capability field.
	if a.JudgeModel == "" {
		a.JudgeModel = suggestModel(models, "judge")
	}
	if a.EmbedModel == "" {
		a.EmbedModel = suggestModel(models, "embed")
	}
	if a.VisionModel == "" {
		a.VisionModel = suggestModel(models, "vl", "vision", "omni")
	}
	if a.AudioModel == "" {
		a.AudioModel = suggestModel(models, "omni", "audio", "ast", "whisper")
	}
	return models, false
}

// modelGroups: one model role per group. A blurred huh select still renders all its options, so stacking
// several overflows and the viewport scrolls options out of sight.
func modelGroups(a *cli.InitAnswers, models []string, manual bool) []*huh.Group {
	none := huh.NewOption("None - disable", "")
	return []*huh.Group{
		huh.NewGroup(selectOrInput(manual, modelOptions(models), &a.MainModel)).
			Title("Main model").Description("The model quack reasons and plans with"),
		huh.NewGroup(specialistSelect("", models, &a.JudgeModel, none)).
			Title("Judge model").Description("Trust gate - scores every node's output"),
		huh.NewGroup(specialistSelect("", models, &a.EmbedModel, none)).
			Title("Embedding model").Description("Semantic memory - None disables it"),
		// Bounded Height keeps two model selects from overflowing one group.
		huh.NewGroup(
			specialistSelect("Vision", models, &a.VisionModel, none),
			specialistSelect("Audio", models, &a.AudioModel, none),
		).Title("Attachment models").Description("Image- and audio-reading - None disables either"),
	}
}

// featureList seeds the optional-feature multi-select from the answer defaults.
func featureList(a *cli.InitAnswers) []string {
	feats := []string{}
	if a.WebSearch {
		feats = append(feats, "search")
	}
	if a.WebFetch {
		feats = append(feats, "fetch")
	}
	return feats
}

// featuresGroup: multi-select of optional tool features; its value drives the search/fetch store groups'
// WithHideFunc, revealing or hiding them live.
func featuresGroup(feats *[]string) *huh.Group {
	return huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Optional features").
			Options(
				huh.NewOption("Web search", "search"),
				huh.NewOption("Web fetch", "fetch"),
				huh.NewOption("Coding agents (workspace + sandbox; needs node + the pi-acp shim)", "coding"),
			).
			Value(feats),
	).Title("Features").Description("Toggle the tool backends to configure")
}

// codingGroups: coder model (default: the main model) plus sandbox mode, only with the coding feature.
// The sandbox default is bwrap if bubblewrap is installed, else none.
func codingGroups(a *cli.InitAnswers, feats *[]string, models []string) []*huh.Group {
	if a.CoderModel == "" {
		a.CoderModel = suggestModel(models, "coder", "code")
	}
	if a.Sandbox == "" {
		a.Sandbox = "none"
		if _, err := exec.LookPath("bwrap"); err == nil {
			a.Sandbox = "bwrap"
		}
	}
	hide := func() bool { return !slices.Contains(*feats, "coding") }
	model := huh.NewGroup(specialistSelect("", models, &a.CoderModel, huh.NewOption("Same as main model", ""))).
		Title("Coder model").Description("Powers code-implementer/explorer/reviewer").
		WithHideFunc(hide)
	sandbox := huh.NewGroup(
		huh.NewSelect[string]().
			Options(
				huh.NewOption("bwrap - OS sandbox for the coding agents' child processes (needs bubblewrap)", "bwrap"),
				huh.NewOption("none  - no OS boundary", "none"),
			).
			Value(&a.Sandbox),
	).Title("Workspace sandbox").Description("The OS boundary agent-run commands execute inside").
		WithHideFunc(hide)
	return []*huh.Group{model, sandbox}
}

// storeGroups: session always, memory with an embedder, search/fetch with their feature, shown live via
// WithHideFunc. Emit gates on the same flags, so a hidden group's default is never written.
func storeGroups(a *cli.InitAnswers, feats *[]string, local bool) []*huh.Group {
	session := storeGroup("Session storage", []string{"sqlite", "postgres"}, &a.SessionKind, &a.SessionURL, "sqlite").
		Description("Where quack keeps its state + tool backends")
	memory := storeGroup("Memory store", []string{"sqlite", "qdrant"}, &a.MemoryKind, &a.MemoryURL, "sqlite").
		WithHideFunc(func() bool { return a.EmbedModel == "" })
	search := storeGroup("Web search backend", []string{"exa", "searxng"}, &a.SearchKind, &a.SearchURL, "exa").
		WithHideFunc(func() bool { return !slices.Contains(*feats, "search") })
	fetch := storeGroup("Web fetch backend", []string{"direct", "crawl4ai"}, &a.FetchKind, &a.FetchURL, "direct").
		WithHideFunc(func() bool { return !slices.Contains(*feats, "fetch") })
	if !local {
		// `quack server init` (the Docker/operator persona) needs the real
		// backends - Postgres, Qdrant, SearXNG, crawl4ai - so keep every screen.
		return []*huh.Group{session, memory, search, fetch}
	}
	// The local persona's defaults (sqlite, exa, direct) are already right -
	// one confirm replaces four screens unless the user opts into choosing.
	useDefaults := true
	confirm := huh.NewGroup(
		huh.NewConfirm().
			Title("Use sqlite for storage and keyless web tools?").
			Description("No = choose backends (Postgres/Qdrant/SearXNG/crawl4ai)").
			Value(&useDefaults),
	).Title("Storage")
	hideDefaults := func() bool { return useDefaults }
	session = session.WithHideFunc(hideDefaults)
	memory = memory.WithHideFunc(func() bool { return useDefaults || a.EmbedModel == "" })
	search = search.WithHideFunc(func() bool { return useDefaults || !slices.Contains(*feats, "search") })
	fetch = fetch.WithHideFunc(func() bool { return useDefaults || !slices.Contains(*feats, "fetch") })
	return []*huh.Group{confirm, session, memory, search, fetch}
}

// storeGroup: a backend select plus a url input left blank; its placeholder tracks the kind's
// cli.DefaultBackendURL, which the emitter fills in when empty.
func storeGroup(title string, kinds []string, kind, url *string, defKind string) *huh.Group {
	*kind = defKind
	opts := make([]huh.Option[string], 0, len(kinds))
	for _, k := range kinds {
		opts = append(opts, huh.NewOption(k, k))
	}
	return huh.NewGroup(
		huh.NewSelect[string]().Title("Backend").Options(opts...).Value(kind),
		huh.NewInput().Title("URL").
			PlaceholderFunc(func() string {
				if d := cli.DefaultBackendURL(*kind); d != "" {
					return d + " (default)"
				}
				return "none needed"
			}, kind).
			Value(url),
	).Title(title)
}

// modelOptions builds select options from a model list. If manual (no models),
// returns nil and the caller uses an Input instead.
func modelOptions(models []string) []huh.Option[string] {
	if len(models) == 0 {
		return nil
	}
	opts := make([]huh.Option[string], 0, len(models))
	for _, m := range models {
		opts = append(opts, huh.NewOption(m, m))
	}
	return opts
}

// selectOrInput: a Select when models were discovered, else an Input. No .Height,
// so the whole option list stays static rather than scrolling.
func selectOrInput(manual bool, opts []huh.Option[string], val *string) huh.Field {
	if manual || len(opts) == 0 {
		return huh.NewInput().Value(val)
	}
	return huh.NewSelect[string]().Options(opts...).Value(val)
}

// specialistSelect is a model role pick with a "None - disable" option;
// an Input when no models were discovered.
func specialistSelect(title string, models []string, val *string, none huh.Option[string]) huh.Field {
	if len(models) == 0 {
		return huh.NewInput().Title(title).Placeholder("blank for none").Value(val)
	}
	opts := append([]huh.Option[string]{none}, modelOptions(models)...)
	// A prefilled value missing from the discovered list (stale env, different IDs) is added as an option
	// so the select still pre-selects it.
	if *val != "" && !slices.Contains(models, *val) {
		opts = append(opts, huh.NewOption(*val, *val))
	}
	return huh.NewSelect[string]().Title(title).Height(8).Options(opts...).Value(val)
}

// suggestModel returns the first model whose name contains any keyword (case-insensitive), or "".
func suggestModel(models []string, keywords ...string) string {
	for _, m := range models {
		low := strings.ToLower(m)
		for _, kw := range keywords {
			if low != "" && kw != "" && strings.Contains(low, strings.ToLower(kw)) {
				return m
			}
		}
	}
	return ""
}

// suggestMain picks a default main model: prefer something that looks like a
// general chat model (not an embedder), else the first available.
func suggestMain(models []string) string {
	for _, m := range models {
		low := strings.ToLower(m)
		if strings.Contains(low, "embed") {
			continue
		}
		return m
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}
