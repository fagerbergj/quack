.PHONY: build run test vet fmt generate frontend-build docker-up docker-down clean

BINARY := quack

## build: build the frontend, embed it, and compile the server
build: frontend-build
	go build -o $(BINARY) ./cmd/server

## frontend-build: build the SPA into the server's embed dir
frontend-build:
	cd frontend && npm ci && npm run build
	rm -rf cmd/server/web/dist
	cp -R frontend/dist cmd/server/web/dist

## run: build and run locally (expects env: DATABASE_URL, LLM_ENDPOINT, ORCH_MODEL)
run: build
	./$(BINARY)

## test: run Go tests
test:
	go test ./...

## vet: go vet
vet:
	go vet ./...

## fmt: gofmt the source
fmt:
	gofmt -w internal cmd

## generate: regenerate Go + TS code from openapi.yaml
generate:
	./scripts/generate.sh

## docker-up: start the full stack (app + self-contained Postgres)
docker-up:
	docker compose up --build

## docker-down: stop the stack
docker-down:
	docker compose down

## clean: remove build artifacts
clean:
	rm -rf frontend/dist $(BINARY)
	git checkout -- cmd/server/web/dist 2>/dev/null || true
