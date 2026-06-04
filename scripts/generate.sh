#!/bin/bash
set -e

echo "Generating Go types from OpenAPI spec..."
oapi-codegen -package schema -generate types,server openapi.yaml > server/api/schema/types.go

echo "Generating TypeScript client from OpenAPI spec..."
cd frontend
npx openapi-ts

echo "Code generation complete!"
