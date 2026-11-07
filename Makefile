# tine build targets. Run `make` or `make help` to list them.
#
# Everything is built on your Mac (or in CI) and run on the Linux dev server.

MODULE   := github.com/sanidhyasin/tine
BINARIES := tine tined tine-agent
OUT      := bin/linux-amd64

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT)

# Keep in sync with .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.15.0
TOOLS                 := $(CURDIR)/.tools
GOLANGCI_LINT         := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  make %-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build every binary for the Linux server into bin/linux-amd64/
	@mkdir -p $(OUT)
	@for b in $(BINARIES); do \
		echo "build $(OUT)/$$b"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(OUT)/$$b ./cmd/$$b || exit 1; \
	done

.PHONY: check
check: vet lint test ## Everything CI checks: vet, lint (including formatting), unit tests

.PHONY: vet
vet: ## go vet, for this machine and for Linux
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## golangci-lint: static checks and formatting, as on Linux
	GOOS=linux GOARCH=amd64 $(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format all Go code (gofmt and goimports)
	$(GOLANGCI_LINT) fmt ./...

.PHONY: test
test: ## Unit tests, with the race detector
	go test -race -count=1 -timeout 3m ./...

# Packages with tests tagged `integration`. They need Linux with KVM, and the
# jailer needs root, so they're compiled here and run on the dev server with sudo.
INTEGRATION_PKGS := ./internal/firecracker ./internal/storage ./internal/jailer ./internal/netns ./internal/image ./internal/vm

.PHONY: test-integration
test-integration: build ## Integration tests on the dev server, as root, against real Firecracker (server must be up)
	@rm -rf $(OUT)/tests && mkdir -p $(OUT)/tests
	@for p in $(INTEGRATION_PKGS); do \
		name=$$(echo $$p | sed 's|^\./||; s|/|-|g'); \
		echo "build $(OUT)/tests/$$name.test"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags integration -o $(OUT)/tests/$$name.test $$p || exit 1; \
	done
	./scripts/dev-server.sh sync
	@# The tests set up and tear down the host's VM networking themselves, so a
	@# running tined would clash with them: they stop it (its VMs must be gone).
	./scripts/dev-server.sh ssh 'sudo systemctl stop tined 2>/dev/null || true; \
		cd ~/tine && for t in $(OUT)/tests/*.test; do echo "=== $$t"; \
		sudo TINE_ARTIFACTS=$$HOME/tine/artifacts TINE_AGENT_BIN=$$HOME/tine/$(OUT)/tine-agent \
		./$$t -test.v -test.count=1 || exit 1; done'

.PHONY: e2e
e2e: deploy ## End-to-end tests against the real tined on the dev server (server must be up; about 2 minutes)
	@mkdir -p $(OUT)/tests
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags e2e -o $(OUT)/tests/e2e.test ./e2e
	./scripts/dev-server.sh sync
	./scripts/dev-server.sh ssh 'cd ~/tine && sudo ./$(OUT)/tests/e2e.test -test.v -test.count=1 -test.timeout 20m'

.PHONY: deploy
deploy: build ## Build, copy to the dev server, install the binaries and the tined service, (re)start it
	./scripts/dev-server.sh sync
	./scripts/dev-server.sh ssh 'set -e; cd ~/tine \
		&& sudo install -m 0755 $(addprefix $(OUT)/,$(BINARIES)) /usr/local/bin/ \
		&& sudo install -m 0644 deploy/tined.service /etc/systemd/system/tined.service \
		&& sudo systemctl daemon-reload && sudo systemctl enable --quiet tined && sudo systemctl restart tined \
		&& echo "tined is $$(systemctl is-active tined): $$(tined version)"'

.PHONY: base-image
base-image: deploy ## Build the "base" image on the dev server, with this tree's tine-agent (server must be up)
	./scripts/dev-server.sh ssh 'cd ~/tine/artifacts && sudo tined image import base --force \
		--kernel "$$(ls vmlinux-* | sort -V | tail -1)" --rootfs "$$(ls ubuntu-*.squashfs | sort -V | tail -1)" \
		&& sudo tined image ls'

.PHONY: clean
clean: ## Remove build output
	rm -rf bin

# The linter is installed into .tools/, pinned to one version, so every
# machine and CI run the same one.
$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS)/golangci-lint $@
