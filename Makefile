.PHONY: build test fmt vet generate generate-server generate-client frontend-build clean

build: frontend-build
	go build -o agent-researcher ./server

frontend-build:
	cd frontend && npm ci && npm run build
	rm -rf server/web/dist
	cp -R frontend/dist server/web/dist

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

generate: generate-server generate-client

generate-server:
	go generate ./server/api/schema/...

generate-client:
	cd frontend && npx openapi-ts

clean:
	rm -rf server/web/dist frontend/dist agent-researcher
