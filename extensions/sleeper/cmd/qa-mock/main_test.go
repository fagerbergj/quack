package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewServerCreatesFixtureDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "get")
	s, err := newServer(dir, false)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if s.dir != dir || s.record {
		t.Errorf("newServer(%q, false) = %+v", dir, s)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected fixture dir to exist: %v", err)
	}
}

func TestServeHTTPRejectsNonGET(t *testing.T) {
	s, err := newServer(t.TempDir(), false)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/state/nfl", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestServeHTTPReplaysFixture(t *testing.T) {
	dir := t.TempDir()
	req := httptest.NewRequest("GET", "/v1/state/nfl", nil)
	key := fixtureKey(req)
	if err := os.WriteFile(filepath.Join(dir, key), []byte(`{"week":2}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := newServer(dir, false)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/state/nfl", nil))
	if rec.Code != 200 || rec.Body.String() != `{"week":2}` {
		t.Errorf("status=%d body=%q, want 200 {\"week\":2}", rec.Code, rec.Body.String())
	}
}

func TestServeHTTPMissingFixtureWithoutRecord(t *testing.T) {
	s, err := newServer(t.TempDir(), false)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/state/nfl", nil))
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestFixtureKeyIgnoresEmptyQuery(t *testing.T) {
	a := fixtureKey(httptest.NewRequest("GET", "/v1/state/nfl", nil))
	b := fixtureKey(httptest.NewRequest("GET", "/v1/state/nfl?", nil))
	if a != b {
		t.Errorf("fixtureKey should ignore an empty query string: %q != %q", a, b)
	}
	c := fixtureKey(httptest.NewRequest("GET", "/v1/state/nfl?week=2", nil))
	if a == c {
		t.Error("fixtureKey should differ when the query string differs")
	}
}
