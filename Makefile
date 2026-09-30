# Karvon backend developer commands.
GO ?= go
GOBIN ?= $(shell $(GO) env GOPATH)/bin
BINDIR ?= bin

# Reacher is a separate container and an opt-in: read README > Licensing before
# switching it on. One variable in .env drives all of it — whether the container
# starts, and whether the stored provider flag is on — so `make dev` brings up the
# same set of connectors the API is configured to talk to.
REACHER_ENABLED := $(shell sed -n 's/^KARVON_VERIFY_REACHER_ENABLED=//p' .env 2>/dev/null | tail -1)
REACHER_ON := $(if $(filter true,$(REACHER_ENABLED)),true,false)

# fb-scrape (services/fb-scrape) is opt-in the same way: a heavy image, and without
# Webshare proxies it scrapes Facebook from this machine's own IP.
FB_SCRAPE_ENABLED := $(shell sed -n 's/^KARVON_FB_SCRAPE_ENABLED=//p' .env 2>/dev/null | tail -1)
FB_SCRAPE_ON := $(if $(filter true,$(FB_SCRAPE_ENABLED)),true,false)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: dev
dev: compose-db compose-reacher compose-fb-scrape migrate verify-settings-sync ## Start Postgres and every enabled connector, migrate, sync provider flags and run the API
	$(GO) run ./cmd/api

.PHONY: run
run: ## Run the API against an already-migrated database
	$(GO) run ./cmd/api

.PHONY: build
build: ## Build both binaries into ./bin
	$(GO) build -trimpath -o $(BINDIR)/karvon-api ./cmd/api
	$(GO) build -trimpath -o $(BINDIR)/karvon-migrate ./cmd/migrate

.PHONY: gen
gen: gen-api gen-sql ## Regenerate the OpenAPI server and the sqlc queries

.PHONY: gen-api
gen-api: ## Regenerate internal/http/gen from api/openapi.yaml
	$(GOBIN)/oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

.PHONY: gen-sql
gen-sql: ## Regenerate internal/db/dbgen from queries/*.sql
	$(GOBIN)/sqlc generate

.PHONY: migrate
migrate: ## Apply all migrations (application schema + queue schema)
	$(GO) run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	$(GO) run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Print migration status
	$(GO) run ./cmd/migrate status

.PHONY: test
test: ## Run unit tests
	$(GO) test ./... -race -count=1

.PHONY: test-integration
test-integration: ## Run unit and integration tests (needs Docker for testcontainers)
	KARVON_INTEGRATION=1 $(GO) test ./... -race -count=1 -timeout 15m

.PHONY: cover
cover: ## Run tests and open a coverage report
	$(GO) test ./... -count=1 -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint
	$(GOBIN)/golangci-lint run

.PHONY: fmt
fmt: ## Format the codebase
	$(GO) fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: check
check: fmt vet lint test ## Everything CI runs

.PHONY: secret
secret: ## Print a fresh KARVON_SECRET_KEY
	@$(GO) run ./cmd/secret

.PHONY: compose-db
compose-db: ## Start only Postgres, and wait for it to accept connections
	docker compose up -d --wait postgres

.PHONY: compose-reacher
compose-reacher: ## Start the Reacher container when .env enables it
ifeq ($(REACHER_ON),true)
	@# --wait holds until the healthcheck passes, so the API does not start calling a
	@# backend that is still booting: five failures inside its start-up window open the
	@# client's circuit breaker and the provider then sits out a whole cooldown. The
	@# failure is not fatal, though — the pipeline is built to survive Reacher being
	@# down, so a backend that never comes up must not stop the API starting.
	docker compose --profile reacher up -d --wait reacher \
		|| echo "WARNING: Reacher did not become healthy; the API will start without it."
else
	@echo "Reacher off (KARVON_VERIFY_REACHER_ENABLED); its container was not started."
endif

.PHONY: compose-reacher-down
compose-reacher-down: ## Stop the Reacher container, leaving the rest of the stack up
	docker compose --profile reacher stop reacher

.PHONY: compose-fb-scrape
compose-fb-scrape: ## Build and start the fb-scrape container when .env enables it
ifeq ($(FB_SCRAPE_ON),true)
	@# --build picks up edits under services/fb-scrape; unchanged, it is a cache hit.
	@# The first build pulls Chromium (~1.3 GB) and takes a few minutes. Not fatal:
	@# the API does not need the scraper to start.
	docker compose --profile fb-scrape up -d --build --wait fb-scrape \
		|| echo "WARNING: fb-scrape did not become healthy; the API will start without it."
else
	@echo "fb-scrape off (KARVON_FB_SCRAPE_ENABLED); its container was not started."
endif

.PHONY: compose-fb-scrape-down
compose-fb-scrape-down: ## Stop the fb-scrape container, leaving the rest of the stack up
	docker compose --profile fb-scrape stop fb-scrape

.PHONY: verify-settings-sync
verify-settings-sync: ## Point the stored Reacher flag at .env (the row, not the env, decides at runtime)
	@docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -qtAX -U karvon -d karvon -c \
		"UPDATE verification_settings SET enabled = enabled || '{\"reacher\": $(REACHER_ON)}'::jsonb, updated_at = now() WHERE id = 1 AND COALESCE(enabled->>'reacher', 'false') <> '$(REACHER_ON)';" > /dev/null
	@echo "verification_settings.enabled.reacher = $(REACHER_ON)"

.PHONY: compose-up
compose-up: ## Start Postgres and the API in Docker
	docker compose up --build -d

.PHONY: compose-down
compose-down: ## Stop the compose stack
	docker compose down

.PHONY: compose-reset
compose-reset: ## Stop the stack and delete the database volume
	docker compose down -v
