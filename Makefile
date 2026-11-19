SHELL := /bin/zsh
COMPOSE_FILE := deployments/docker/docker-compose.yml

.PHONY: help up down logs ps migrate-up migrate-down migrate-status keys bootstrap token build test test-integration vet run-api run-scheduler run-worker clean

# Load .env when present so local targets share the API's configuration.
ifneq (,$(wildcard .env))
include .env
export
endif

# Local RS256 dev keypair (see `make keys`). Absolute paths so targets that cd still find them.
KEY_DIR := $(CURDIR)/.secrets
export SYSTERA_AUTH_PRIVATE_KEY_FILE ?= $(KEY_DIR)/auth_private.pem
ifeq ($(SYSTERA_AUTH_JWKS_URL)$(SYSTERA_AUTH_PUBLIC_KEY_PEM),)
export SYSTERA_AUTH_PUBLIC_KEY_FILE ?= $(KEY_DIR)/auth_public.pem
endif

TEST_DATABASE_URL ?= postgres://$(or $(SYSTERA_POSTGRES_USER),systera):$(or $(SYSTERA_POSTGRES_PASSWORD),systera_dev_password)@localhost:$(or $(SYSTERA_POSTGRES_PORT),5432)/$(or $(SYSTERA_POSTGRES_DB),systera)?sslmode=disable

help:
	@echo "Systera — Stage 0 Foundation Targets"
	@echo "  make up           Start local infrastructure (Postgres+pgvector, Redis, NATS, MinIO)"
	@echo "  make down         Stop local infrastructure"
	@echo "  make ps           Show container status"
	@echo "  make logs         Tail infrastructure logs"
	@echo "  make migrate-up   Apply pending PostgreSQL migrations (embedded, versioned)"
	@echo "  make migrate-down Revert the most recent migration"
	@echo "  make keys         Generate a local RS256 dev keypair in .secrets/"
	@echo "  make bootstrap    Create a tenant + owner (SLUG=acme EMAIL=you@acme.test)"
	@echo "  make token        Mint an access token (EMAIL=you@acme.test [TTL=1h])"
	@echo "  make build        Build all Go control-plane and worker binaries"
	@echo "  make test         Run Go unit and tenant-isolation tests"
	@echo "  make test-integration  Run tests against the local PostgreSQL (make up first)"
	@echo "  make vet          Run go vet across backend"
	@echo "  make run-api      Run the Go Control Plane API locally"
	@echo "  make run-scheduler  Run the outbox relay + lease reclaimer (needs NATS: make up)"
	@echo "  make run-worker   Run the specialist agent worker (needs a model endpoint, e.g. Ollama)"

up:
	docker compose -f $(COMPOSE_FILE) up -d

down:
	docker compose -f $(COMPOSE_FILE) down

ps:
	docker compose -f $(COMPOSE_FILE) ps

logs:
	docker compose -f $(COMPOSE_FILE) logs -f

migrate-up:
	cd control-plane && go run ./cmd/migrate up

migrate-down:
	cd control-plane && go run ./cmd/migrate down 1

migrate-status:
	cd control-plane && go run ./cmd/migrate status

keys:
	@mkdir -p $(KEY_DIR) && chmod 700 $(KEY_DIR)
	@test -f $(KEY_DIR)/auth_private.pem || openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out $(KEY_DIR)/auth_private.pem 2>/dev/null
	@chmod 600 $(KEY_DIR)/auth_private.pem
	@openssl pkey -in $(KEY_DIR)/auth_private.pem -pubout -out $(KEY_DIR)/auth_public.pem
	@echo "keypair ready in $(KEY_DIR)"

bootstrap:
	@test -n "$(SLUG)" -a -n "$(EMAIL)" || (echo "usage: make bootstrap SLUG=acme EMAIL=you@acme.test [NAME='You'] [ROLE=OWNER]" && exit 2)
	cd control-plane && go run ./cmd/systera-admin bootstrap -tenant-slug "$(SLUG)" -email "$(EMAIL)" -name "$(or $(NAME),$(EMAIL))" -role "$(or $(ROLE),OWNER)"

token:
	@test -n "$(EMAIL)" || (echo "usage: make token EMAIL=you@acme.test [TTL=1h]" && exit 2)
	@cd control-plane && go run ./cmd/systera-admin token -subject "$(EMAIL)" -ttl "$(or $(TTL),1h)"

build:
	cd control-plane && go build -o ../bin/ ./cmd/...

test:
	cd control-plane && go test -race ./...

test-integration:
	cd control-plane && SYSTERA_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" SYSTERA_TEST_NATS_URL="$(or $(TEST_NATS_URL),nats://localhost:4222)" go test -race -count=1 -v ./test/...

vet:
	cd control-plane && go vet ./...

run-api:
	cd control-plane && go run ./cmd/api

run-scheduler:
	cd control-plane && go run ./cmd/scheduler

run-worker:
	cd control-plane && go run ./cmd/worker-agent

clean:
	rm -rf bin/
