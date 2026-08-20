package serve

import (
	"log/slog"
	"sort"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// extTool pairs an extension/plugin tool with the provider name it came from,
// so name collisions can be disambiguated as <provider>_<tool> (the same
// underscore convention quackmcp_* uses).
type extTool struct {
	provider string
	tool     tool.Tool
}

// runnableExtTool is the subset a tool must implement to be renamed: the
// prefixed alias has to declare and pack itself under the new name.
type renamableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
}

// renamedTool exposes an existing tool under a different name, everywhere the
// name matters: Name(), the packed declaration, and request processing.
type renamedTool struct {
	renamableTool
	name string
}

func (r *renamedTool) Name() string { return r.name }

func (r *renamedTool) Declaration() *genai.FunctionDeclaration {
	d := *r.renamableTool.Declaration()
	d.Name = r.name
	return &d
}

// ProcessRequest packs the wrapper, never the inner tool, so the model sees
// the prefixed name and calls route back through this wrapper.
func (r *renamedTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, r)
}

// indexExtTools builds the by-name lookup agents' tools: lists resolve
// against. Every tool is addressable as <provider>_<name>; the bare name
// resolves too when exactly one provider supplies it. A collided bare name
// maps to nil - a sentinel tools.Build turns into an "ambiguous" error - so
// no provider silently shadows another.
func indexExtTools(exts []extTool) map[string]tool.Tool {
	byName := make(map[string]tool.Tool, len(exts))
	providers := make(map[string][]string) // bare name -> provider names
	for _, e := range exts {
		bare := e.tool.Name()
		providers[bare] = append(providers[bare], e.provider)

		prefixed := e.provider + "_" + bare
		if rt, ok := e.tool.(renamableTool); ok {
			byName[prefixed] = &renamedTool{renamableTool: rt, name: prefixed}
		} else {
			slog.Warn("extension tool cannot be renamed; prefixed form unavailable",
				"component", "startup", "tool", bare, "provider", e.provider)
		}

		if _, dup := byName[bare]; dup {
			byName[bare] = nil // ambiguous: bare-name use is a config error
			continue
		}
		byName[bare] = e.tool
	}
	for bare, provs := range providers {
		if len(provs) < 2 {
			continue
		}
		sort.Strings(provs)
		prefixed := make([]string, len(provs))
		for i, p := range provs {
			prefixed[i] = p + "_" + bare
		}
		slog.Warn("extension tool name collision; bare name is ambiguous, use a prefixed form",
			"component", "startup", "tool", bare, "prefixed_forms", prefixed)
	}
	return byName
}
