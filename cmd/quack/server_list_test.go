package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/cli"
)

// TestServerList_JSON covers cli.md audit finding 12: `server list --json`
// (previously it printed `* name  url` for a human to parse by hand).
func TestServerList_JSON(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	rc, err := cli.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.AddServer("a", "http://a"); err != nil {
		t.Fatal(err)
	}
	if err := rc.AddServer("b", "http://b"); err != nil {
		t.Fatal(err)
	}
	if err := rc.Use("b"); err != nil {
		t.Fatal(err)
	}
	if err := rc.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := newServerListCmd()
	c.SetOut(&out)
	c.SetArgs([]string{"--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("server list --json: %v", err)
	}

	var rows []serverListRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	want := map[string]serverListRow{
		"a": {Name: "a", URL: "http://a", Active: false},
		"b": {Name: "b", URL: "http://b", Active: true},
	}
	for _, r := range rows {
		if want[r.Name] != r {
			t.Errorf("row %+v, want %+v", r, want[r.Name])
		}
	}
}

// TestServerList_JSONEmpty covers the empty-registry case: `[]`, not the
// human "no servers registered" message mixed into --json output.
func TestServerList_JSONEmpty(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	var out bytes.Buffer
	c := newServerListCmd()
	c.SetOut(&out)
	c.SetArgs([]string{"--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("server list --json: %v", err)
	}
	if got := bytes.TrimSpace(out.Bytes()); string(got) != "[]" {
		t.Errorf("empty registry --json = %q, want []", got)
	}
}
