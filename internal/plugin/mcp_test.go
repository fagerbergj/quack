package plugin

import (
	"path/filepath"
	"testing"
)

func mcpRoot(t *testing.T, mcpJSON string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), `{"$schema":"x","name":"p"}`)
	if mcpJSON != "" {
		writeFile(t, filepath.Join(root, "mcp.json"), mcpJSON)
	}
	return root
}

func resolveOne(t *testing.T, root string) Plugin {
	t.Helper()
	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d plugins, want 1", len(got))
	}
	return got[0]
}

func TestLoadMCP_StdioEntry(t *testing.T) {
	p := resolveOne(t, mcpRoot(t, `{
		"$schema":"`+mcpSchemaID+`",
		"mcpServers":{"validator":{"type":"stdio","command":"./bin/validator","args":["--data","${PLUGIN_DATA}/v"],"env":{"CONFIG":"${PLUGIN_ROOT}/c.json"}}}
	}`))
	s, ok := p.MCPServers["validator"]
	if !ok {
		t.Fatalf("MCPServers = %+v, want a validator entry", p.MCPServers)
	}
	if s.Command != "./bin/validator" {
		t.Errorf("Command = %q", s.Command)
	}
}

// §7.2.2 rule 2: a broken or version-mismatched mcp.json disables MCP for
// that plugin only - the plugin itself still loads.
func TestLoadMCP_BadFileDisablesMCPOnly(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `{nope`,
		"wrong schema":   `{"$schema":"https://agent-plugins.org/schemas/9.9.9/mcp.schema.json","mcpServers":{}}`,
		"extra top key":  `{"$schema":"` + mcpSchemaID + `","mcpServers":{},"surprise":1}`,
		"missing schema": `{"mcpServers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := resolveOne(t, mcpRoot(t, body))
			if len(p.MCPServers) != 0 {
				t.Errorf("MCPServers = %+v, want none", p.MCPServers)
			}
		})
	}
}

// §7.2.2 rules 3 and 4: one bad or unsupported entry costs only itself.
func TestLoadMCP_BadEntrySkippedOthersKept(t *testing.T) {
	p := resolveOne(t, mcpRoot(t, `{
		"$schema":"`+mcpSchemaID+`",
		"mcpServers":{
			"good":{"type":"stdio","command":"validator"},
			"shellish":{"type":"stdio","command":"sh -c evil"},
			"absolute":{"type":"stdio","command":"/usr/bin/evil"},
			"reserved":{"type":"stdio","command":"v","env":{"PLUGIN_ROOT":"/etc"}},
			"mixed":{"type":"stdio","command":"v","url":"https://x.example"},
			"unknown":{"type":"carrier-pigeon","command":"v"},
			"remote":{"type":"streamable-http","url":"https://x.example/mcp"}
		}
	}`))
	if len(p.MCPServers) != 1 {
		t.Fatalf("MCPServers = %+v, want only good", p.MCPServers)
	}
	if _, ok := p.MCPServers["good"]; !ok {
		t.Errorf("MCPServers = %+v, want the good entry kept", p.MCPServers)
	}
}

func TestLaunch_ExpandsPlaceholdersAndSuppliesReservedVars(t *testing.T) {
	s := MCPServer{
		Command: "./bin/v",
		Args:    []string{"--data", "${PLUGIN_DATA}/x", "--root", "${PLUGIN_ROOT}", "--literal", "${NOPE}"},
		Env:     map[string]string{"CONFIG": "${PLUGIN_ROOT}/c.json"},
		Cwd:     "${PLUGIN_DATA}/work",
	}
	argv, env, cwd, err := s.Launch("/p/root", "/p/data")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	want := []string{"/p/root/bin/v", "--data", "/p/data/x", "--root", "/p/root", "--literal", "${NOPE}"}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
	if cwd != "/p/data/work" {
		t.Errorf("cwd = %q, want /p/data/work", cwd)
	}
	// §9.1: the client supplies both reserved variables, last so they win.
	var root, data bool
	for _, e := range env {
		switch e {
		case "PLUGIN_ROOT=/p/root":
			root = true
		case "PLUGIN_DATA=/p/data":
			data = true
		}
	}
	if !root || !data {
		t.Errorf("env = %v, want PLUGIN_ROOT and PLUGIN_DATA supplied", env)
	}
}

func TestLaunch_RejectsEscapingPaths(t *testing.T) {
	for name, s := range map[string]MCPServer{
		"command escapes":  {Command: "./../../bin/sh"},
		"cwd escapes data": {Command: "v", Cwd: "${PLUGIN_DATA}/../../etc"},
		"cwd absolute":     {Command: "v", Cwd: "/etc"},
		// The plugin root is read-only, so a cwd there is refused rather than
		// silently made writable - see MCPServer.Launch.
		"cwd at plugin root":    {Command: "v", Cwd: "${PLUGIN_ROOT}"},
		"cwd plugin-relative":   {Command: "v", Cwd: "./work"},
		"cwd under plugin root": {Command: "v", Cwd: "${PLUGIN_ROOT}/work"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := s.Launch("/p/root", "/p/data"); err == nil {
				t.Fatal("Launch = nil error, want a cwd error")
			}
		})
	}
}

// An omitted cwd lands in PLUGIN_DATA, not the plugin root: a child's working
// directory is necessarily writable, and the root must stay read-only.
func TestLaunch_DefaultCwdIsPluginData(t *testing.T) {
	_, _, cwd, err := MCPServer{Command: "v"}.Launch("/p/root", "/p/data")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if cwd != "/p/data" {
		t.Errorf("cwd = %q, want /p/data", cwd)
	}
}
