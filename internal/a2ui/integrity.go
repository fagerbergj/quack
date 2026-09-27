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

// checkIntegrity: unique ids, a root, every reference resolves to a component
// with exactly one parent, no cycles, non-empty ChoicePickers, http(s)-only URLs.
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
	for _, c := range comps {
		if err := checkContent(c); err != nil {
			return err
		}
	}
	return checkCycles(comps, byID)
}

func checkRefs(comps []Component, byID map[string]Component) error {
	parent := map[string]string{}
	for _, c := range comps {
		for _, r := range refs(c) {
			if _, ok := byID[r]; !ok {
				return fmt.Errorf("component %q references missing component %q", idOf(c), r)
			}
			if p, ok := parent[r]; ok && p == idOf(c) {
				return fmt.Errorf("component %q lists child %q twice", p, r)
			} else if ok {
				return fmt.Errorf("component %q is referenced by both %q and %q; a component has one parent - give the copy its own id", r, p, idOf(c))
			}
			parent[r] = idOf(c)
		}
	}
	return nil
}

func checkCycles(comps []Component, byID map[string]Component) error {
	state := make(map[string]int, len(comps)) // 1 = on the DFS stack, 2 = done
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("component %q is its own ancestor (reference cycle)", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, r := range refs(byID[id]) {
			if err := visit(r); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, c := range comps {
		if err := visit(idOf(c)); err != nil {
			return err
		}
	}
	return nil
}

// checkContent: what the schema can't say - a ChoicePicker needs options, and
// literal media URLs and openUrl targets must be http(s) (no javascript:/data:).
func checkContent(c Component) error {
	switch c["component"] {
	case "ChoicePicker":
		if opts, _ := c["options"].([]any); len(opts) == 0 {
			return fmt.Errorf("component %q: ChoicePicker needs at least one option", idOf(c))
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
