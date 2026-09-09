.PHONY: build test test-spike test-integration compose-up compose-down compose-logs bootstrap-admin gen-certs install-buf proto-lint proto-gen tidy

BUF := $(shell go env GOPATH)/bin/buf

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

tidy:
	go mod tidy

# buf is installed as a standalone binary (not an in-module go tool) to avoid
# pulling its large dependency tree into this module's go.mod/go.sum.
install-buf:
	go install github.com/bufbuild/buf/cmd/buf@v1.72.0

proto-lint:
	$(BUF) lint

proto-gen: proto-lint
	$(BUF) generate
