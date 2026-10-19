# Two classes of target, kept apart on purpose.
#
# The Go targets need nothing but a Go toolchain and never touch the network:
# dependencies are vendored, so `make test` works on a clean clone and
# offline, which is what SPEC.md 20 requires.
#
# The Docker targets need a daemon, and the first run of a containerised one
# pulls an image. Use them when you have no local Go, or when you want the
# build to be reproducible rather than fast.

GO_DOCKER := docker compose run --rm go
PG_USER  := $${POSTGRES_USER:-robasko}
PG_DB    := $${POSTGRES_DB:-robasko}

.DEFAULT_GOAL := help
.PHONY: help test vet fmt check hooks migrate up db down logs psql run test-docker vet-docker

help: ## List the available targets
	@echo "Go only (no Docker, no network):"
	@grep -E '^[a-z-]+:.*?## .*\[go\]' $(MAKEFILE_LIST) \
		| sed 's/:.*## /\t/; s/ \[go\]//' | sort | awk -F'\t' '{printf "  %-14s %s\n", $$1, $$2}'
	@echo
	@echo "Docker:"
	@grep -E '^[a-z-]+:.*?## .*\[docker\]' $(MAKEFILE_LIST) \
		| sed 's/:.*## /\t/; s/ \[docker\]//' | sort | awk -F'\t' '{printf "  %-14s %s\n", $$1, $$2}'

# --------------------------------------------------------------------------
# Go toolchain only
# --------------------------------------------------------------------------

test: ## Run the tests, offline [go]
	go test ./...

vet: ## Report suspect constructs [go]
	go vet ./...

fmt: ## Format the source tree [go]
	go fmt ./...

migrate: ## Apply pending migrations from the host [go]
	go run ./cmd/fetch -migrate

check: ## Audit every tracked file for protected literals [go]
	go run ./cmd/protect -all

hooks: ## Install the pre-commit hook. Safe to re-run [go]
	git config core.hooksPath .githooks
	@echo "hooks installed: $$(git config core.hooksPath)"

# --------------------------------------------------------------------------
# Docker
# --------------------------------------------------------------------------

up: ## Start the stack, apply migrations, wait until healthy [docker]
	docker compose up -d --build --wait
	@echo "postgres  localhost:$${POSTGRES_PORT:-5432}"
	@echo "agentd    localhost:$${AGENTD_PORT:-8080}/healthz"
	@echo "schema    applied by agentd at startup; see 'make logs'"

db: ## Start only Postgres [docker]
	docker compose up -d --wait db

down: ## Stop the stack, keeping the data volume [docker]
	docker compose down

logs: ## Follow the stack's logs [docker]
	docker compose logs -f --tail=50

psql: ## Open a psql shell against the running database [docker]
	docker compose exec db psql -U $(PG_USER) -d $(PG_DB)

run: ## Run the stack in the foreground [docker]
	docker compose up --build

test-docker: ## Test in a container, no local Go needed [docker]
	$(GO_DOCKER) test ./...

vet-docker: ## Vet in a container [docker]
	$(GO_DOCKER) vet ./...
