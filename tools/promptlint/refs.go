package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/vetting"
)

var (
	linkRe      = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
	loadSkillRe = regexp.MustCompile(`load_skill\("([a-zA-Z0-9_:-]+)"\)`)
	resourceRe  = regexp.MustCompile(`(?:^|[\s(\x60"'])((?:references|assets|scripts)/[\w./-]*\w)`)
	// A prompt naming a plugin skill's file: `sleeper:waivers`'s `references/output-schema.json`.
	skillFileRe = regexp.MustCompile("`([a-z0-9-]+:[a-z0-9-]+)`'s `([^`]+)`")
	codeSpanRe  = regexp.MustCompile("`[^`\n]*`")
)

// skillDir resolves a skills: name to its in-tree directory, "" when no tree ships it.
func skillDir(root, name string) string {
	cands := []string{"skills/" + name, ".agents/embedded/dotagents/skills/" + name}
	if plugin, skill, ok := strings.Cut(name, ":"); ok {
		cands = []string{"extensions/" + plugin + "/plugin/skills/" + skill}
	}
	for _, c := range cands {
		if fileSize(root, c+"/SKILL.md") > 0 {
			return c
		}
	}
	return ""
}

// frontmatter is the SKILL.md header the skill toolset lists to every agent offered the skill.
func frontmatter(root, name string) string {
	dir := skillDir(root, name)
	if dir == "" {
		return ""
	}
	raw, _ := os.ReadFile(filepath.Join(root, dir, "SKILL.md"))
	parts := strings.SplitN(string(raw), "---", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// checkSkill fails an unresolvable skill; a registry-fetched plugin's skill only warns, since no tree has it.
func (l *linter) checkSkill(subject, name string, sources []string) {
	if skillDir(l.root, name) != "" {
		return
	}
	plugin, _, _ := strings.Cut(name, ":")
	external := slices.Contains(l.cfg.ExternalSkills, plugin) || slices.Contains(l.cfg.ExternalSkills, name)
	l.add(finding{Rule: "unknown_skill", Subject: subject, warn: external, sources: sources,
		Message: fmt.Sprintf("skill %q resolves under no skills/, dotagents or extensions/*/plugin/skills dir", name)})
}

func (l *linter) checkRefs() {
	files := textFiles(l.root)
	for _, rel := range sortedKeys(files) {
		raw, err := os.ReadFile(filepath.Join(l.root, rel))
		if err != nil {
			continue
		}
		if strings.HasSuffix(rel, ".yaml") {
			l.checkRubric(rel, raw)
			continue
		}
		text := stripFences(string(raw))
		l.checkLinks(rel, text)
		for _, m := range loadSkillRe.FindAllStringSubmatch(text, -1) {
			l.checkSkill(rel, m[1], []string{rel})
		}
		l.checkResources(rel, text)
	}
}

// stripFences drops fenced code blocks, whose links and paths are examples, not references.
func stripFences(s string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if !in {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// checkLinks skips links inside code spans: `[text](url)` is an example of the syntax.
func (l *linter) checkLinks(rel, text string) {
	for _, m := range linkRe.FindAllStringSubmatch(codeSpanRe.ReplaceAllString(text, ""), -1) {
		target, _, _ := strings.Cut(m[1], "#")
		if target == "" || strings.Contains(target, ":") || strings.ContainsAny(target, "<{*") {
			continue
		}
		if fileSize(l.root, path.Join(path.Dir(rel), target)) == 0 && !isDir(l.root, path.Join(path.Dir(rel), target)) {
			l.add(finding{Rule: "missing_link", Subject: rel, sources: []string{rel},
				Message: fmt.Sprintf("link target %q does not exist", m[1])})
		}
	}
}

// checkResources resolves a SKILL.md's references/assets/scripts paths against its own dir, and a
// prompt's `plugin:skill`'s `file` against that skill.
func (l *linter) checkResources(rel, text string) {
	var refs [][2]string // {skill dir, path}
	if strings.HasSuffix(rel, "/SKILL.md") {
		for _, m := range resourceRe.FindAllStringSubmatch(text, -1) {
			refs = append(refs, [2]string{path.Dir(rel), m[1]})
		}
	}
	for _, m := range skillFileRe.FindAllStringSubmatch(text, -1) {
		dir := skillDir(l.root, m[1])
		if dir == "" {
			l.checkSkill(rel, m[1], []string{rel})
			continue
		}
		refs = append(refs, [2]string{dir, m[2]})
	}
	for _, r := range refs {
		p := path.Join(r[0], r[1])
		if fileSize(l.root, p) == 0 && !isDir(l.root, p) {
			l.add(finding{Rule: "missing_resource", Subject: rel, sources: []string{rel, r[0] + "/"},
				Message: fmt.Sprintf("skill resource %q does not exist", p)})
		}
	}
}

func isDir(root, rel string) bool {
	st, err := os.Stat(filepath.Join(root, rel))
	return err == nil && st.IsDir()
}

// checkRubric runs the loader serve uses, then fails a deterministic criterion code never computes.
func (l *linter) checkRubric(rel string, raw []byte) {
	if _, err := vetting.LoadReplayRubric(context.Background(), nil, "", filepath.Join(l.root, rel)); err != nil {
		l.add(finding{Rule: "rubric_invalid", Subject: rel, Message: err.Error(), sources: []string{rel}})
		return
	}
	var doc struct {
		Criteria map[string]struct {
			Deterministic bool `yaml:"deterministic"`
		} `yaml:"criteria"`
	}
	_ = yaml.Unmarshal(raw, &doc)
	owned := vetting.CodeOwnedCriteria()
	for _, name := range sortedKeys(doc.Criteria) {
		if doc.Criteria[name].Deterministic && !slices.Contains(owned, name) {
			l.add(finding{Rule: "rubric_undefined_criterion", Subject: rel, sources: []string{rel, "internal/vetting/"},
				Message: fmt.Sprintf("criterion %q is deterministic: true but no code computes it, so it is never scored", name)})
		}
	}
}
