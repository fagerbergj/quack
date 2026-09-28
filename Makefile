.PHONY: build run test vet fmt generate frontend-build docker-up docker-down clean docs-check slop dev-build dev-image dev-check dev-work

BINARY := quack
SANDBOX_BINARY := quack-sandbox

## build: build the frontend, embed it, and compile the server + sandbox sidecar
build: frontend-build
	go build -o $(BINARY) ./cmd/quack
	go build -o $(SANDBOX_BINARY) ./cmd/quack-sandbox

## frontend-build: build the SPA into the server's embed dir
frontend-build:
	cd frontend && npm ci && npm run build
	rm -rf internal/serve/web/dist
	cp -R frontend/dist internal/serve/web/dist
	touch internal/serve/web/dist/.gitkeep   # keep the embed placeholder tracked

## dev-build: build quack + sandbox against a local quack-extensions checkout (EXT=../quack-extensions); go.mod untouched
dev-build: dev-work
	GOWORK=$(DEV_GOWORK) go build -ldflags "-X main.version=$(DEV_VERSION)" -o $(BINARY) ./cmd/quack
	GOWORK=$(DEV_GOWORK) go build -o $(SANDBOX_BINARY) ./cmd/quack-sandbox

## dev-image: build the Docker image against EXT's modules, tagged $(DEV_IMAGE)
dev-image: dev-check
	docker build --build-context ext=$(EXT) --build-arg QUACK_SRC=backend-src-ext \
	  --build-arg VERSION=$(DEV_VERSION) -t $(DEV_IMAGE) .

DEV_IMAGE ?= quack:dev
DEV_GOWORK := $(CURDIR)/.dev/go.work
DEV_VERSION = dev+ext.$(shell git -C $(EXT) describe --always --dirty --exclude='*' 2>/dev/null)

dev-check:
	@test -n "$(wildcard $(EXT)/*/go.mod)" || { echo "EXT='$(EXT)' has no */go.mod module dirs; set EXT to a quack-extensions checkout, e.g. make $(MAKECMDGOALS) EXT=../quack-extensions" >&2; exit 1; }

# Throwaway go.work kept out of the repo root, so plain go/make commands never pick it up.
dev-work: dev-check
	rm -f $(DEV_GOWORK) $(DEV_GOWORK).sum && mkdir -p $(dir $(DEV_GOWORK))
	GOWORK=$(DEV_GOWORK) go work init $(CURDIR) $(abspath $(dir $(wildcard $(EXT)/*/go.mod)))

## run: build and run locally (expects env: QUACK_DATABASE_URL, QUACK_LLM_ENDPOINT, QUACK_ORCH_MODEL)
run: build
	./$(BINARY) server run --config config/quack.yaml

## test: run Go tests
test:
	go test ./...

## test-race: run Go tests under the race detector (what CI gates on)
test-race:
	go test -race ./...

## vet: go vet
vet:
	go vet ./...

## docs-check: markdown lint + doc drift (links, paths, config keys, hard wraps)
docs-check:
	npm --prefix scripts exec -- markdownlint-cli2 'docs/**/*.md' 'README.md'
	node scripts/check-docs.mjs
	node scripts/unwrap-md.mjs --check $$(find docs -name '*.md') *.md

## fmt: gofmt the source
fmt:
	gofmt -w internal cmd

## slop: repo ledger - CC/dup/concentration/test-ratio (report-only; gate is CI go-slop)
slop:
	go run ./tools/sloplint repo

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
	rm -rf frontend/dist $(BINARY) $(SANDBOX_BINARY)
	rm -rf internal/serve/web/dist
	mkdir -p internal/serve/web/dist
	touch internal/serve/web/dist/.gitkeep
