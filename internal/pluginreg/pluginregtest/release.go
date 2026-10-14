package pluginregtest

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ReleaseServer stands in for GitHub release downloads: files["<tag>/<asset>"] is served at /<tag>/<asset>.
func ReleaseServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// SHA256SUMS is assets' sha256sum listing, the checksum file a release publishes.
func SHA256SUMS(assets map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range assets {
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(data), name)
	}
	return []byte(b.String())
}
