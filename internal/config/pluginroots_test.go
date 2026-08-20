package config

import "testing"

// plugins: supersedes skills.plugins, but an existing quack.yaml spelling it
// the old way must keep working - the rename is a deprecation, not a break.
func TestPluginRoots(t *testing.T) {
	cases := map[string]struct {
		cfg  Config
		want []string
	}{
		"neither set uses defaults": {Config{}, defaultSkillPlugins},
		"top-level plugins wins":    {Config{Plugins: []string{"a"}, Skills: SkillsConfig{Plugins: []string{"b"}}}, []string{"a"}},
		"deprecated alias read":     {Config{Skills: SkillsConfig{Plugins: []string{"b"}}}, []string{"b"}},
		"explicit empty is honored": {Config{Plugins: []string{}}, []string{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.cfg.PluginRoots()
			if len(got) != len(tc.want) {
				t.Fatalf("PluginRoots() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("PluginRoots() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
