package langfuse

import (
	"context"
	"net/http"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
)

// NewGenClient builds the generated dataset client for baseURL (datasets,
// dataset items, dataset run items - client.go doesn't cover these). opts is
// the hand-written Client's Option set; WithPinLabel doesn't apply, ignored.
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
