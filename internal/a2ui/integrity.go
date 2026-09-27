package a2ui

import (
	"errors"
	"fmt"
	"net/url"
)

// refs lists the component ids c points at, per the catalog's ComponentId/ChildList fields.
func refs(c Component) []string {
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	switch c["component"] {
	case "Card", "Button":
		add(c["child"])
	case "Modal":
		add(c["trigger"])
		add(c["content"])
	case "Row", "Column", "List":
		switch ch := c["children"].(type) {
		case []any:
			for _, v := range ch {
				add(v)
			}
		case map[string]any:
			add(ch["componentId"])
		}
	case "Tabs":
		tabs, _ := c["tabs"].([]any)
		for _, t := range tabs {
			if m, ok := t.(map[string]any); ok {
				add(m["child"])
			}
		}
	}
	return out
}

// checkIntegrity: unique ids and a tree - every reference resolves, each component but
// root has exactly one parent and is reachable from root (which rules out cycles).
func checkIntegrity(comps []Component) error {
	byID := make(map[string]Component, len(comps))
	for _, c := range comps {
		id := idOf(c)
		if _, dup := byID[id]; dup {
			return fmt.Errorf("duplicate component id %q", id)
		}
		byID[id] = c
	}
	if _, ok := byID["root"]; !ok {
		return errors.New(`no component has id "root"`)
	}
	if err := checkRefs(comps, byID); err != nil {
		return err
	}
	if err := checkReachable(comps, byID); err != nil {
		return err
	}
	for _, c := range comps {
		if err := checkContent(c); err != nil {
			return err
		}
	}
	return nil
}

func checkRefs(comps []Component, byID map[string]Component) error {
	parent := map[string]string{}
	for _, c := range comps {
		for _, r := range refs(c) {
			if _, ok := byID[r]; !ok {
				return fmt.Errorf("component %q references missing component %q", idOf(c), r)
			}
			p, seen := parent[r]
			switch {
			case r == idOf(c):
				return fmt.Errorf("component %q references itself (a cycle)", r)
			case r == "root":
				return fmt.Errorf(`component %q references "root"; root is the top of the tree and has no parent`, idOf(c))
			case seen && p == idOf(c):
				return fmt.Errorf("component %q lists child %q twice", p, r)
			case seen:
				return fmt.Errorf("component %q is referenced by both %q and %q; a component has one parent - give the copy its own id", r, p, idOf(c))
			}
			parent[r] = idOf(c)
		}
	}
	return nil
}

// checkReachable rejects components no path from root reaches; Merge never deletes, so orphans would pile up.
func checkReachable(comps []Component, byID map[string]Component) error {
	seen := map[string]bool{"root": true}
	for queue := []string{"root"}; len(queue) > 0; queue = queue[1:] {
		for _, r := range refs(byID[queue[0]]) {
			if !seen[r] {
				seen[r] = true
				queue = append(queue, r)
			}
		}
	}
	var orphans []string
	for _, c := range comps {
		if !seen[idOf(c)] {
			orphans = append(orphans, idOf(c))
		}
	}
	if len(orphans) > 0 {
		return fmt.Errorf("components %q are not reachable from root; reference each from a parent (e.g. add it to a Column's children) in the same call", orphans)
	}
	return nil
}

// checkContent: what the schema can't say - a ChoicePicker needs options, and
// literal media URLs and openUrl targets must be http(s) (no javascript:/data:).
func checkContent(c Component) error {
	switch c["component"] {
	case "ChoicePicker":
		if err := checkPicker(c); err != nil {
			return err
		}
	case "Image", "Video", "AudioPlayer":
		if s, ok := c["url"].(string); ok && !httpURL(s) {
			return fmt.Errorf("component %q: url %q must be an absolute http(s) URL", idOf(c), s)
		}
	}
	if s, ok := findOpenURL(c); ok {
		return fmt.Errorf("component %q: openUrl target %q must be an absolute http(s) URL", idOf(c), s)
	}
	return nil
}

// checkPicker: options non-empty with distinct labels - a quiz key identifies its answer by label.
func checkPicker(c Component) error {
	opts, _ := c["options"].([]any)
	if len(opts) == 0 {
		return fmt.Errorf("component %q: ChoicePicker needs at least one option", idOf(c))
	}
	seen := map[string]bool{}
	for _, o := range opts {
		m, _ := o.(map[string]any)
		l := text(m["label"])
		if seen[l] {
			return fmt.Errorf("component %q: two options share the label %s; each option needs its own label", idOf(c), l)
		}
		seen[l] = true
	}
	return nil
}

// findOpenURL returns the first literal openUrl url under v that is not http(s).
func findOpenURL(v any) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		if t["call"] == "openUrl" {
			args, _ := t["args"].(map[string]any)
			if s, ok := args["url"].(string); ok && !httpURL(s) {
				return s, true
			}
		}
		for _, x := range t {
			if s, ok := findOpenURL(x); ok {
				return s, true
			}
		}
	case []any:
		for _, x := range t {
			if s, ok := findOpenURL(x); ok {
				return s, true
			}
		}
	}
	return "", false
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
