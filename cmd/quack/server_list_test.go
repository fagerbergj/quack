package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/cli"
)

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

// TestServerList_ShowsVersion covers the human-readable path: a reachable
// server's GET /api/v1/config version is appended to its row.
func TestServerList_ShowsVersion(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"0.51.26"}`)
	}))
	defer srv.Close()

	rc, err := cli.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.AddServer("a", srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := rc.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := newServerListCmd()
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("server list: %v", err)
	}
	if !strings.Contains(out.String(), "0.51.26") {
		t.Errorf("output = %q, want it to contain the reachable server's version", out.String())
	}
}

// TestServerList_SkipsVersionOverTenServers: past 10 registered servers the
// per-server version lookup (a live request each) is skipped, not attempted -
// the list stays readable and the command doesn't stall on a slow registry.
func TestServerList_SkipsVersionOverTenServers(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	rc, err := cli.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 11; i++ {
		if err := rc.AddServer(fmt.Sprintf("s%d", i), fmt.Sprintf("http://s%d.example", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := rc.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := newServerListCmd()
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("server list: %v", err)
	}
	if !strings.Contains(out.String(), "skipping version lookup") {
		t.Errorf("output = %q, want a note that version lookup was skipped", out.String())
	}
}
