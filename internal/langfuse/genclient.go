package langfuse

import (
	"context"
	"net/http"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
)

// NewGenClient builds the generated dataset/score client for baseURL, with the same
// Basic auth (public_key:secret_key) and httpx transport as the hand-written Client -
// additive, only for endpoints (datasets, dataset items/run items, scores) client.go doesn't cover.
func NewGenClient(baseURL, publicKey, secretKey string, opts ...Option) (*langfusegen.ClientWithResponses, error) {
	c := New(baseURL, publicKey, secretKey, opts...)
	auth := func(_ context.Context, req *http.Request) error {
		req.SetBasicAuth(c.publicKey, c.secretKey)
		return nil
	}
	return langfusegen.NewClientWithResponses(c.baseURL,
		langfusegen.WithHTTPClient(c.httpClient),
		langfusegen.WithRequestEditorFn(auth))
}
