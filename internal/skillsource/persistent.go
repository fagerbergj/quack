package skillsource

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"
)

// loadedSkillsKey: chat-scoped, not a process map, so a resumed session
// behaves the same as one still in memory.
const loadedSkillsKey = "quack.loaded_skills"

// Persistent wraps an ADK skill toolset so a loaded skill's body rides the
// system instruction instead of a conversation turn that trimming or compaction can lose. source must build the same skills as inner.
func Persistent(inner *skilltoolset.SkillToolset, source skill.Source) tool.Toolset {
	return &persistentToolset{inner: inner, source: source}
}

type persistentToolset struct {
	inner  *skilltoolset.SkillToolset
	source skill.Source
}

func (p *persistentToolset) Name() string { return p.inner.Name() }

func (p *persistentToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := p.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, len(tools))
	for i, t := range tools {
		if t.Name() == "load_skill" {
			out[i] = p.loadSkillTool()
			continue
		}
		out[i] = t
	}
	return out, nil
}

// ProcessRequest folds in every skill loaded in a PRIOR invocation; the
// current invocation's own load is skipped since its tool result already carries the body, and adding it here too would send it twice.
func (p *persistentToolset) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := p.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	loaded := loadedSkillsFrom(ctx.ReadonlyState())
	if len(loaded) == 0 {
		return nil
	}
	names := make([]string, 0, len(loaded))
	for name, invocationID := range loaded {
		if invocationID == ctx.InvocationID() {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names) // stable order: an unstable one breaks the prefix cache on every call

	var sb strings.Builder
	sb.WriteString("The skills below were already loaded earlier in this session. Follow their instructions; do not call load_skill for them again.\n")
	for _, name := range names {
		instructions, err := p.source.LoadInstructions(ctx, name)
		if err != nil {
			continue // renamed or removed since it was loaded
		}
		fmt.Fprintf(&sb, "\n<skill name=%q>\n%s\n</skill>\n", name, instructions)
	}
	appendInstruction(req, sb.String())
	return nil
}

// appendInstruction replicates ADK's internal utils.AppendInstructions (which
// we can't import): append a block to the request's system instruction.
func appendInstruction(r *model.LLMRequest, inst string) {
	if r.Config == nil {
		r.Config = &genai.GenerateContentConfig{}
	}
	si := r.Config.SystemInstruction
	if si == nil {
		r.Config.SystemInstruction = genai.NewContentFromText(inst, genai.RoleUser)
		return
	}
	if n := len(si.Parts); n > 0 && si.Parts[n-1].Text != "" {
		si.Parts[n-1].Text += "\n\n" + inst
		return
	}
	si.Parts = append(si.Parts, genai.NewPartFromText(inst))
}

func loadedSkillsFrom(state session.ReadonlyState) map[string]string {
	v, err := state.Get(loadedSkillsKey)
	if err != nil {
		return nil
	}
	raw, _ := v.(string)
	if raw == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// markSkillLoaded is a no-op if name is already recorded: the first
// invocation to load a skill owns the invocation id used for dedup above.
func markSkillLoaded(state session.State, name, invocationID string) error {
	m := loadedSkillsFrom(state)
	if _, ok := m[name]; ok {
		return nil
	}
	if m == nil {
		m = map[string]string{}
	}
	m[name] = invocationID
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return state.Set(loadedSkillsKey, string(b))
}

type loadSkillArgs struct {
	Name string `json:"name" jsonschema:"The name of the skill to load."`
}

// frontmatterJSON mirrors ADK's internal skilltool.FrontmatterJSON (json tags
// this package can't reuse since that type is unexported from an internal package).
type frontmatterJSON struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AllowedTools  []string          `json:"allowed-tools,omitempty"`
}

type loadSkillResult struct {
	SkillName    string           `json:"skill_name,omitempty"`
	Instructions string           `json:"instructions,omitempty"`
	Frontmatter  *frontmatterJSON `json:"frontmatter,omitempty"`
}

func (p *persistentToolset) loadSkillTool() tool.Tool {
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "load_skill",
			Description: "Loads the SKILL.md instructions for a given skill.",
		},
		func(ctx agent.Context, args loadSkillArgs) (*loadSkillResult, error) {
			return p.runLoadSkill(ctx, args)
		},
	)
	if err != nil {
		// Config and the handler's arg/result types are fixed above and never
		// change at runtime, so functiontool.New cannot fail here.
		panic(err)
	}
	return t
}

func (p *persistentToolset) runLoadSkill(ctx agent.Context, args loadSkillArgs) (*loadSkillResult, error) {
	if args.Name == "" {
		return nil, fmt.Errorf("skill name is required to load a skill")
	}
	if loaded := loadedSkillsFrom(ctx.ReadonlyState()); loaded != nil {
		if _, ok := loaded[args.Name]; ok {
			return &loadSkillResult{
				SkillName:    args.Name,
				Instructions: fmt.Sprintf("Skill %q is already loaded; its instructions are in the system prompt.", args.Name),
			}, nil
		}
	}
	fm, err := p.source.LoadFrontmatter(ctx, args.Name)
	if err != nil {
		return nil, fmt.Errorf("load frontmatter for skill %q: %w", args.Name, err)
	}
	instructions, err := p.source.LoadInstructions(ctx, args.Name)
	if err != nil {
		return nil, fmt.Errorf("load instructions for skill %q: %w", args.Name, err)
	}
	if err := markSkillLoaded(ctx.State(), args.Name, ctx.InvocationID()); err != nil {
		return nil, fmt.Errorf("record loaded skill %q: %w", args.Name, err)
	}
	return &loadSkillResult{
		SkillName:    args.Name,
		Instructions: instructions,
		Frontmatter: &frontmatterJSON{
			Name:          fm.Name,
			Description:   fm.Description,
			License:       fm.License,
			Compatibility: fm.Compatibility,
			Metadata:      fm.Metadata,
			AllowedTools:  fm.AllowedTools,
		},
	}, nil
}
