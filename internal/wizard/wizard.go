// Package wizard is the interactive `quack init` / `quack server init` surface:
// Huh forms that collect answers and hand them to internal/cli to emit config
// or update the client registry. The forms are thin — all logic (model
// discovery, YAML emit, registry save) lives in cli so it's testable without a
// terminal. Per the quack-cli skill, you test the emitted YAML, not the
// keystrokes.
package wizard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/fagerbergj/quack/internal/cli"
)

// ServerInit runs the server-config wizard and writes quack.yaml at outPath.
// It's `quack server init` (and the local branch of `quack init`): LLM provider
// → endpoint → /models → model roles → optional features → stores.
func ServerInit(ctx context.Context, outPath string, force bool) error {
	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", outPath)
		}
	}

	a := cli.InitAnswers{
		WebSearch: true, // optional features default on; the user can turn them off
		WebFetch:  true,
	}
	cli.PrefillFromEnv(&a) // don't re-ask what the environment already answers
	if err := askProvider(ctx, &a); err != nil {
		return err
	}
	if err := askModels(ctx, &a); err != nil {
		return err
	}
	if err := askFeatures(&a); err != nil {
		return err
	}
	if err := askStores(&a); err != nil {
		return err
	}

	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(outPath, []byte(cli.EmitServerConfig(a)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("\n✓ Wrote %s\n", outPath)
	for _, line := range cli.EnvExports(a) {
		fmt.Println(line)
	}
	fmt.Println("\nRun `quack server run` to start.")
	return nil
}

// ClientInit is `quack init`: local (run quack here) or remote (connect to one).
// Local runs ServerInit then registers localhost; remote just registers.
func ClientInit(ctx context.Context, serverInitPath string, force bool) error {
	var mode string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("How will you use quack?").
			Options(
				huh.NewOption("Local — run quack on this machine", "local"),
				huh.NewOption("Remote — connect to a server someone else runs", "remote"),
			).
			Value(&mode),
	)).Run(); err != nil {
		return err
	}

	switch mode {
	case "local":
		if err := ServerInit(ctx, serverInitPath, force); err != nil {
			return err
		}
		c, err := cli.LoadClient()
		if err != nil {
			return err
		}
		_ = c.AddServer("local", "http://localhost:8080") // ignore "exists" — local is conventional
		if err := c.Use("local"); err != nil {
			return err
		}
		if err := c.Save(); err != nil {
			return err
		}
		fmt.Println("✓ Registered `local` as your active server.")
		return nil
	case "remote":
		return registerRemote()
	default:
		return nil
	}
}

func registerRemote() error {
	var name, url string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Server name").Value(&name),
		huh.NewInput().Title("Server URL (e.g. https://quack.example.com)").Value(&url),
	)).Run(); err != nil {
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

// askProvider: the LLM provider (single-option, OpenAI-compatible today),
// endpoint, and API key — one group so the three are navigable together.
func askProvider(ctx context.Context, a *cli.InitAnswers) error {
	var kind string
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("LLM provider").
			Options(huh.NewOption("OpenAI-compatible", "openai")).
			Value(&kind),
		huh.NewInput().Title("Endpoint (e.g. http://localhost:11436/v1)").Value(&a.Endpoint),
		huh.NewInput().Title("API key (blank if none)").Value(&a.APIKey),
	)).Run()
}

// askModels calls /models, then asks for the main model + specialist models as
// one multi-group form — so shift+tab/left returns to an earlier role. If
// /models fails, it falls back to manual text entry for each role.
func askModels(ctx context.Context, a *cli.InitAnswers) error {
	models, err := cli.ListModels(ctx, a.Endpoint, a.APIKey)
	manual := err != nil
	if manual {
		fmt.Fprintf(os.Stderr, "couldn't reach %s/models (%v) — entering model names manually.\n", a.Endpoint, err)
	}

	if a.MainModel == "" && !manual {
		a.MainModel = suggestMain(models)
	}
	// Heuristic pre-selections for specialist roles (overridable; None disables).
	// The OpenAI /models response gives IDs only — no capability field — so we
	// guess from the name. Fast in the common case, easy to change when wrong.
	if a.JudgeModel == "" {
		a.JudgeModel = suggestModel(models, "gemma", "judge")
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

	none := huh.NewOption("None — disable", "")
	return huh.NewForm(
		huh.NewGroup(selectOrInput(manual, "Main chat model (orchestrator + researcher)", modelOptions(models, a.MainModel), &a.MainModel)),
		huh.NewGroup(specialistSelect("Judge model (trust gate)", models, &a.JudgeModel, none)),
		huh.NewGroup(specialistSelect("Embedding model (semantic memory)", models, &a.EmbedModel, none)),
		huh.NewGroup(specialistSelect("Vision model (image-reader)", models, &a.VisionModel, none)),
		huh.NewGroup(specialistSelect("Audio model (media-reader)", models, &a.AudioModel, none)),
	).Run()
}

// askFeatures: multi-select of optional tool features. Web search/fetch drive
// whether their store questions appear in askStores.
func askFeatures(a *cli.InitAnswers) error {
	feats := []string{}
	if a.WebSearch {
		feats = append(feats, "search")
	}
	if a.WebFetch {
		feats = append(feats, "fetch")
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Optional features").
			Options(
				huh.NewOption("Web search", "search"),
				huh.NewOption("Web fetch", "fetch"),
			).
			Value(&feats),
	))
	if err := form.Run(); err != nil {
		return err
	}
	a.WebSearch = slices.Contains(feats, "search")
	a.WebFetch = slices.Contains(feats, "fetch")
	return nil
}

// askStores: one multi-group form — session (always), memory (if embedder),
// search (if web search), fetch (if web fetch) — so shift+tab/left returns to
// an earlier store. Each store is a group of kind-select + url-input; the url
// is blank/ignored for kinds that need none (exa/direct).
func askStores(a *cli.InitAnswers) error {
	groups := []*huh.Group{
		storeGroup("Session storage", []string{"sqlite", "postgres"}, &a.SessionKind, &a.SessionURL, "sqlite", "quack.db"),
	}
	if a.EmbedModel != "" {
		groups = append(groups, storeGroup("Memory store", []string{"sqlite", "qdrant"}, &a.MemoryKind, &a.MemoryURL, "sqlite", "quack.db"))
	}
	if a.WebSearch {
		groups = append(groups, storeGroup("Web search backend", []string{"exa", "searxng"}, &a.SearchKind, &a.SearchURL, "exa", ""))
	}
	if a.WebFetch {
		groups = append(groups, storeGroup("Web fetch backend", []string{"direct", "crawl4ai"}, &a.FetchKind, &a.FetchURL, "direct", ""))
	}
	return huh.NewForm(groups...).Run()
}

// storeGroup builds one group: a kind select + a url input, pre-filled with the
// defaults for the initial kind. The url is always shown (blank for kinds that
// need none); the user edits it when they pick a url-needing kind.
func storeGroup(title string, kinds []string, kind, url *string, defKind, defURL string) *huh.Group {
	*kind = defKind
	*url = defURL
	opts := make([]huh.Option[string], 0, len(kinds))
	for _, k := range kinds {
		opts = append(opts, huh.NewOption(k, k))
	}
	return huh.NewGroup(
		huh.NewSelect[string]().Title(title).Options(opts...).Value(kind),
		huh.NewInput().Title(title+" URL (blank if none)").Value(url),
	)
}

// modelOptions builds select options from a model list. If manual (no models),
// returns nil and the caller uses an Input instead.
func modelOptions(models []string, _ string) []huh.Option[string] {
	if len(models) == 0 {
		return nil
	}
	opts := make([]huh.Option[string], 0, len(models))
	for _, m := range models {
		opts = append(opts, huh.NewOption(m, m))
	}
	return opts
}

// selectOrInput returns a Select field when models were discovered, else an
// Input field for manual entry.
func selectOrInput(manual bool, title string, opts []huh.Option[string], val *string) huh.Field {
	if manual || len(opts) == 0 {
		return huh.NewInput().Title(title).Value(val)
	}
	return huh.NewSelect[string]().Title(title).Options(opts...).Value(val)
}

// specialistSelect is a model role pick with a "None — disable" option (so the
// user can skip judge/memory/vision/audio). Falls back to Input when no models
// were discovered.
func specialistSelect(title string, models []string, val *string, none huh.Option[string]) huh.Field {
	if len(models) == 0 {
		return huh.NewInput().Title(title + " (blank for none)").Value(val)
	}
	opts := append([]huh.Option[string]{none}, modelOptions(models, *val)...)
	// If the prefilled value (from env/heuristic) isn't in the discovered list
	// (stale env, or /models returned different IDs), add it as an explicit
	// option so the select still pre-selects it instead of silently mismatching.
	if *val != "" && !slices.Contains(models, *val) {
		opts = append(opts, huh.NewOption(*val, *val))
	}
	return huh.NewSelect[string]().Title(title).Options(opts...).Value(val)
}

// suggestModel returns the first model whose name contains any of the keywords
// (case-insensitive), or "" if none match. Used to pre-select specialist roles
// so the common case is confirm-confirm-confirm.
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
