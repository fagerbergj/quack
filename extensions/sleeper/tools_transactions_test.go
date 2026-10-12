package sleeper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetTransactions(t *testing.T) {
	e := testExtension(t)
	got, err := e.getTransactions(context.Background(), transactionsArgs{WeeksBack: 2})
	if err != nil {
		t.Fatalf("getTransactions: %v", err)
	}
	if len(got.Transactions) == 0 {
		t.Fatal("expected transactions across weeks 1-2")
	}
	var sawDEF bool
	for _, tx := range got.Transactions {
		if tx.Type == "" || tx.Status == "" {
			t.Errorf("transaction %+v missing type/status", tx)
		}
		if len(tx.Teams) == 0 {
			t.Errorf("transaction %+v has no team names resolved", tx)
		}
		for _, m := range append(append([]moveEntry{}, tx.Adds...), tx.Drops...) {
			if m.Name == "" || m.Name == m.PlayerID {
				t.Errorf("move %+v did not resolve a player name", m)
			}
			if m.Position == "" {
				t.Errorf("move %+v did not resolve a position", m)
			}
			// A team-defense id (e.g. "BAL") must label position "DEF", not
			// read as an unrelated mechanic - see TestGetTransactionsLabelsDefensePosition.
			if m.Position == "DEF" {
				sawDEF = true
			}
		}
	}
	if !sawDEF {
		t.Fatal("expected at least one DEF move in weeks 1-2 fixtures (test assumption changed)")
	}
}

// TestGetTransactionsLabelsDefensePosition: a DEF move ("BAL") resolves to "Baltimore Ravens" / "DEF",
// like sleeper_free_agents, not a bare team code.
func TestGetTransactionsLabelsDefensePosition(t *testing.T) {
	e := testExtension(t)
	got, err := e.getTransactions(context.Background(), transactionsArgs{WeeksBack: 2})
	if err != nil {
		t.Fatalf("getTransactions: %v", err)
	}
	var found *moveEntry
	for _, tx := range got.Transactions {
		for _, m := range append(append([]moveEntry{}, tx.Adds...), tx.Drops...) {
			if m.PlayerID == "BAL" {
				found = &m
			}
		}
	}
	if found == nil {
		t.Fatal("expected a BAL (Ravens DEF) move in weeks 1-2 fixtures (test assumption changed)")
	}
	if found.Position != "DEF" {
		t.Errorf("BAL move position = %q, want DEF", found.Position)
	}
	if found.Name != "Baltimore Ravens" {
		t.Errorf("BAL move name = %q, want Baltimore Ravens", found.Name)
	}
}

// TestGetTransactionsPropagatesGenuineError: a 500 for one round fails the tool; only a 404 round is skipped.
func TestGetTransactionsPropagatesGenuineError(t *testing.T) {
	failWeek := 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, fmt.Sprintf("/transactions/%d", failWeek)) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		key, err := fixtureKey(r.URL.RequestURI())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", "get", key))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	e := &extension{client: client, cfg: config{DefaultUser: testUser, DefaultLeague: testLeague}}
	if _, err := e.getTransactions(context.Background(), transactionsArgs{WeeksBack: 2}); err == nil {
		t.Fatal("expected getTransactions to return the week-2 server error, not swallow it")
	}
}

func TestGetFreeAgents(t *testing.T) {
	e := testExtension(t)
	got, err := e.getFreeAgents(context.Background(), freeAgentsArgs{Position: "RB", Limit: 5})
	if err != nil {
		t.Fatalf("getFreeAgents: %v", err)
	}
	if len(got.FreeAgents) == 0 {
		t.Fatal("expected some RB free agents")
	}
	if len(got.FreeAgents) > 5 {
		t.Errorf("free_agents = %d, want <=5 (limit)", len(got.FreeAgents))
	}
	rosters, err := e.client.Rosters(context.Background(), testLeague)
	if err != nil {
		t.Fatalf("Rosters: %v", err)
	}
	taken := rostered(rosters)
	for i, fa := range got.FreeAgents {
		if fa.Position != "RB" {
			t.Errorf("free agent %+v has position %q, want RB", fa, fa.Position)
		}
		if taken[fa.PlayerID] {
			t.Errorf("free agent %+v is rostered, should be excluded", fa)
		}
		if i > 0 && fa.Projection > got.FreeAgents[i-1].Projection {
			t.Errorf("free agents not sorted by projection desc at index %d", i)
		}
	}
}
