# Targets are listed in one place by `make help`, each saying what it needs
# where that is not obvious.
#
# They were split into "Go only" and "Docker" sections for a while. The
# split read well and was wrong: `hooks` is a git config change that needs
# no Go, and `migrate` needs a running Postgres, which means Docker. A
# grouping that has to stay true of every row drifts; a note on the row
# that needs one cannot be wrong about its neighbours.

GO_DOCKER := docker compose run --rm go
PG_USER  := $${POSTGRES_USER:-robasko}
PG_DB    := $${POSTGRES_DB:-robasko}

.DEFAULT_GOAL := help
.PHONY: help test vet fmt check hooks migrate score up db down logs psql run test-docker vet-docker

help: ## List the available targets
	@grep -E '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| sed 's/:.*## /\t/' | sort | awk -F'\t' '{printf "  %-14s %s\n", $$1, $$2}'

test: ## Run the tests. No network, no database
	go test ./...

vet: ## Report suspect constructs
	go vet ./...

fmt: ## Format the source tree
	go fmt ./...

score: ## Score stored vacancies with the model. Needs Postgres and credits
	go run ./cmd/score

migrate: ## Apply migrations from the host. Needs Postgres running
	go run ./cmd/fetch -migrate

check: ## Audit every tracked file for protected literals
	go run ./cmd/protect -all

hooks: ## Install the pre-commit hook. Git only, safe to re-run
	git config core.hooksPath .githooks
	@echo "hooks installed: $$(git config core.hooksPath)"

up: ## Start the stack, apply migrations, wait until healthy. Docker
	docker compose up -d --build --wait
	@echo "postgres  localhost:$${POSTGRES_PORT:-5432}"
	@echo "agentd    localhost:$${AGENTD_PORT:-8080}/healthz"
	@echo "schema    applied by agentd at startup; see 'make logs'"

db: ## Start only Postgres. Docker
	docker compose up -d --wait db

down: ## Stop the stack, keeping the data volume. Docker
	docker compose down

logs: ## Follow the stack's logs. Docker
	docker compose logs -f --tail=50

psql: ## Open a psql shell against the running database. Docker
	docker compose exec db psql -U $(PG_USER) -d $(PG_DB)

run: ## Run the stack in the foreground. Docker
	docker compose up --build

test-docker: ## Run the tests in a container. No local Go needed
	$(GO_DOCKER) test ./...

vet-docker: ## Vet in a container. No local Go needed
	$(GO_DOCKER) vet ./...
