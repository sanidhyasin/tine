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
	go test -race -count=1 ./...

.PHONY: deploy
deploy: build ## Build, copy to the dev server, install the binaries, restart tined if it's installed
	./scripts/dev-server.sh sync
	./scripts/dev-server.sh ssh 'sudo install -m 0755 ~/tine/$(OUT)/* /usr/local/bin/ \
		&& if systemctl cat tined >/dev/null 2>&1; then sudo systemctl restart tined; fi \
		&& tine version'

.PHONY: clean
clean: ## Remove build output
	rm -rf bin

# The linter is installed into .tools/, pinned to one version, so every
# machine and CI run the same one.
$(GOLANGCI_LINT):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS)/golangci-lint $@
