.PHONY: build test test-spike test-integration test-e2e test-launcher-kvm compose-up compose-down compose-logs bootstrap-admin migrate seed gen-certs install-buf proto-lint proto-gen tidy

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
	go test -tags e2e ./internal/control-plane/e2e/... -v

tidy:
	go mod tidy

# KVM-gated: needs a Linux host with hardware virtualization, root (jailer
# requires it), and the real jailer/firecracker binaries + golden image env
# vars cmd/container-launcher/config.go reads (LAUNCHER_JAILER_BINARY,
# LAUNCHER_FIRECRACKER_BINARY, LAUNCHER_CHROOT_BASE_DIR, LAUNCHER_IMAGE_ROOT
# — see docs/components/container-launcher.md §6). Fails fast, before ever
# invoking go test, if /dev/kvm isn't present/writable — this can't run on
# standard hosted CI runners or the macOS dev loop (design
# docs/designs/completed/container-launcher-design.md §13), only a
# self-hosted or nested-virtualization-enabled one.
test-launcher-kvm:
	@test -e /dev/kvm || { echo "test-launcher-kvm: /dev/kvm not found — this target requires a Linux host with hardware virtualization"; exit 1; }
	@test -r /dev/kvm && test -w /dev/kvm || { echo "test-launcher-kvm: /dev/kvm exists but isn't readable/writable by $$(whoami) — add this user to the kvm group (or run with sufficient privileges)"; exit 1; }
	go test -tags launcherkvm ./cmd/container-launcher/... -v

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
