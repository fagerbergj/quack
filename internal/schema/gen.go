//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config cfg.yaml ../../openapi.yaml

// Package schema holds the OpenAPI-generated server interface and models.
// The generated file (quack.gen.go) is the source of truth for request/response
// types and routing; regenerate with `go generate ./internal/schema/...`.
package schema
