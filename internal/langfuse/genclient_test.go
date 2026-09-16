package langfuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewGenClient_AppliesBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, err := NewGenClient(srv.URL, "pub", "secret", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewGenClient: %v", err)
	}
	if _, err := c.DatasetsGetWithResponse(context.Background(), "x"); err != nil {
		t.Fatalf("DatasetsGetWithResponse: %v", err)
	}
	if !gotOK || gotUser != "pub" || gotPass != "secret" {
		t.Fatalf("basic auth = (%q, %q, %v), want (pub, secret, true)", gotUser, gotPass, gotOK)
	}
}
