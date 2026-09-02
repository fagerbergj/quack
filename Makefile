.PHONY: build run test vet fmt generate frontend-build plugins plugins-update docker-up docker-down clean

BINARY := quack

## plugins: fetch the skill-library plugin trees pinned in .agents/vendor/plugins.yaml
## (not in git; go:embed needs them present, so build/test/run depend on this)
plugins:
	./scripts/plugins.sh $(PLUGIN)

## plugins-update: move each plugin's pin to its upstream HEAD
plugins-update:
	./scripts/plugins.sh --update $(PLUGIN)

## build: build the frontend, embed it, and compile the server
build: plugins frontend-build
	go build -o $(BINARY) ./cmd/quack

## frontend-build: build the SPA into the server's embed dir
frontend-build:
	cd frontend && npm ci && npm run build
	rm -rf internal/serve/web/dist
	cp -R frontend/dist internal/serve/web/dist
	touch internal/serve/web/dist/.gitkeep   # keep the embed placeholder tracked

## run: build and run locally (expects env: QUACK_DATABASE_URL, QUACK_LLM_ENDPOINT, QUACK_ORCH_MODEL)
run: build
	./$(BINARY) --config config/quack.yaml

## test: run Go tests
test: plugins
	go test ./...

## test-race: run Go tests under the race detector (what CI gates on)
test-race: plugins
	go test -race ./...

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
	rm -rf internal/serve/web/dist
	mkdir -p internal/serve/web/dist
	touch internal/serve/web/dist/.gitkeep
