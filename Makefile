.PHONY: env infra local-roles migrate migration-smoke up down test test-race test-integration fmt-check locale-check vet build verify sqlc sqlc-check lint vuln oidc-smoke oidc-smoke-no-build image-manifest clock-preflight stage0-preflight stage0-readonly demo-local demo-check demo-reset demo-down publication-gate
COMPOSE ?= $(shell if docker compose version >/dev/null 2>&1; then echo 'docker compose'; else echo 'docker-compose'; fi)
env:
	sh scripts/dev-env.sh
infra: env
	$(COMPOSE) up -d postgres nats redis
local-roles: infra
	COMPOSE_COMMAND='$(COMPOSE)' sh scripts/ensure-local-roles.sh
migrate: local-roles
	$(COMPOSE) build migrate
	$(COMPOSE) run --rm migrate
migration-smoke: migrate
	python3 scripts/migration_smoke.py
up: local-roles
	$(COMPOSE) up --build -d
down:
	$(COMPOSE) down
test:
	go test ./...
test-race:
	go test -race ./...
test-integration: migrate
	RELAYDESK_TEST_ENV_FILE=.env go test -race -tags integration -count=1 -v ./tests/integration
fmt-check:
	test -z "$$(gofmt -l cmd internal tests scripts/*.go)"
locale-check:
	python3 scripts/check_locale.py
vet:
	go vet ./...
lint:
	golangci-lint run ./...
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
build:
	go build ./cmd/...
sqlc:
	sqlc generate
sqlc-check:
	@tmp=$$(mktemp -d); cp -R internal/dbgen "$$tmp/dbgen"; trap 'rm -rf "$$tmp"' EXIT; sqlc generate; diff -qr "$$tmp/dbgen" internal/dbgen
oidc-smoke: up
	python3 scripts/oidc_smoke.py
image-manifest:
	$(COMPOSE) build api worker migrate
	python3 scripts/validated_images.py record
oidc-smoke-no-build: local-roles
	python3 scripts/validated_images.py check
	$(COMPOSE) up -d --no-build
	python3 scripts/oidc_smoke.py
clock-preflight:
	python3 scripts/clock_preflight.py
stage0-preflight: clock-preflight
	python3 scripts/stage0_probe.py
stage0-readonly: clock-preflight
	python3 scripts/stage0_probe.py --read-only-live
demo-local:
	python3 scripts/demo_local.py start
demo-check:
	python3 scripts/demo_local.py check
demo-reset:
	python3 scripts/demo_local.py reset
demo-down:
	python3 scripts/demo_local.py down
publication-gate:
	python3 scripts/test_safe_publish.py
	python3 scripts/license_notice.py
	python3 scripts/safe_publish.py
verify: fmt-check locale-check vet lint build test test-race sqlc-check test-integration migration-smoke vuln publication-gate
