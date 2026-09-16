//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.0 -config langfusegen/genconfig.yaml openapi.yml

// Package langfuse also vendors a generated client (see langfusegen) for the
// dataset endpoints; regenerate with `go generate ./internal/langfuse/...`.
package langfuse
