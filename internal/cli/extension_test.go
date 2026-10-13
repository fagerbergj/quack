package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunExtensionRestart(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/extensions/github/restart" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no enabled extension has this name"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunExtensionRestart(context.Background(), &out, srv.URL, "github"); err != nil || out.String() != "restarted extension github\n" {
		t.Errorf("restart github = %q, %v", out.String(), err)
	}
	if err := RunExtensionRestart(context.Background(), &out, srv.URL, "nope"); err == nil || !strings.Contains(err.Error(), "no enabled extension") {
		t.Errorf("restart nope = %v, want the server's 404 message", err)
	}
}
