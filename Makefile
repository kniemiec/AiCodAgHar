.PHONY: build test test-spike test-integration test-e2e compose-up compose-down compose-logs bootstrap-admin migrate seed gen-certs install-buf proto-lint proto-gen tidy

BUF := buf

build:
	go build ./...

test:
	go test ./...

test-spike:
	set -a; . deploy/.env; set +a; \
	go test -tags spike ./internal/clients/gitclient/... -v

test-integration:
	set -a; . deploy/.env; set +a; \
	TLS_CA_CERT=$(CURDIR)/deploy/certs/ca.crt \
	TLS_CLIENT_CERT=$(CURDIR)/deploy/certs/client.crt \
	TLS_CLIENT_KEY=$(CURDIR)/deploy/certs/client.key \
	go test -tags integration ./... -v

gen-certs:
	./deploy/gen-certs.sh

compose-up: gen-certs
	docker compose -f deploy/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker-compose.yml down

compose-logs:
	docker compose -f deploy/docker-compose.yml logs -f

bootstrap-admin:
	./deploy/bootstrap-admin.sh

migrate:
	./deploy/migrate.sh

seed:
	set -a; . deploy/.env; set +a; \
	go run ./cmd/seed
	docker compose -f deploy/docker-compose.yml up -d secrets-mock pdp-mock control-plane

test-e2e:
	set -a; . deploy/.env; set +a; \
	TLS_CA_CERT=$(CURDIR)/deploy/certs/ca.crt \
	TLS_CLIENT_CERT=$(CURDIR)/deploy/certs/client.crt \
	TLS_CLIENT_KEY=$(CURDIR)/deploy/certs/client.key \
	go test -tags e2e ./internal/e2e/... -v

tidy:
	go mod tidy

# buf is installed as a standalone binary (not an in-module go tool) to avoid
# pulling its large dependency tree into this module's go.mod/go.sum.
# If you use mise (see .mise.toml), `mise install` gets you the same pinned
# buf version alongside the repo's pinned Go — this target is the fallback
# for anyone not using mise.
install-buf:
	go install github.com/bufbuild/buf/cmd/buf@v1.72.0

proto-lint:
	$(BUF) lint

proto-gen: proto-lint
	$(BUF) generate
