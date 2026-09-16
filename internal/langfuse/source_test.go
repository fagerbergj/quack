package langfuse

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

func TestSourceGet(t *testing.T) {
	c := testClient(t, rawPrompt(`{"name":"system/foo","version":5,"type":"text","prompt":"hi","config":{"model":"m1"}}`))
	src := &Source{Client: c, StoreKey: "langfuse"}
	art, ok, err := src.Get(context.Background(), "system/foo")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if art.Body != "hi" || art.Source != SourceName || art.VersionID != "5" || art.Config["model"] != "m1" {
		t.Fatalf("got %+v", art)
	}
}

func TestSourceGetNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	src := &Source{Client: c, StoreKey: "langfuse"}
	_, ok, err := src.Get(context.Background(), "system/foo")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestSourceGetAuthError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	src := &Source{Client: c, StoreKey: "prod"}
	_, ok, err := src.Get(context.Background(), "system/foo")
	if ok || err == nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "stores.prod credentials") {
		t.Fatalf("err = %v, want a stores.prod credentials hint", err)
	}
}

func TestSourceSeedActions(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rawPrompt(`{"name":"system/foo","version":1,"type":"text","prompt":"x"}`)(w, r)
	})
	src := &Source{Client: c, StoreKey: "langfuse"}
	if err := src.Seed(context.Background(), "system/foo", artifactsrc.Artifact{Body: "body", VersionID: "hash1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}
