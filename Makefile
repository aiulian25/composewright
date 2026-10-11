SHELL := /bin/bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

TOOLS                 := $(CURDIR)/bin/tools
GOLANGCI_LINT_VERSION := v2.14.0
GO_LICENSES_VERSION   := v2.0.1
GOVULNCHECK_VERSION   := v1.8.0

IMAGE ?= composewright:dev

# Licence allowlist. modernc.org/mathutil carries BSD-3-Clause text that
# go-licenses cannot classify (verified by hand).
ALLOWED_LICENSES := Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,0BSD,MPL-2.0,Unlicense,CC0-1.0
LICENSE_IGNORE   := --ignore modernc.org/mathutil
# go-licenses recognises the standard library by its own build-time GOROOT;
# when go.mod's toolchain line switches Go, it must be told the active one.
TOOLCHAIN_ROOT   := $(shell go env GOROOT 2>/dev/null)

.PHONY: all lint test build image container-smoke eval test-docker egress offline dev fmt vuln docs licenses clean tools

all: lint test build

lint: $(TOOLS)/golangci-lint $(TOOLS)/go-licenses
	$(TOOLS)/golangci-lint run ./...
	@for script in scripts/lint-*.sh; do [ -e "$$script" ] || continue; echo "$$script"; "$$script" || exit 1; done
	GOROOT=$(TOOLCHAIN_ROOT) $(TOOLS)/go-licenses check ./... --allowed_licenses=$(ALLOWED_LICENSES) $(LICENSE_IGNORE)

test:
	go test -race -count=1 ./...

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/composewright ./cmd/composewright

# The container image for this machine's platform (Dockerfile).
image:
	docker buildx build --load --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE) .

# Runs the image as the README does: the UI on an empty volume, then init,
# doctor, gen, show and export inside it.
container-smoke: image
	scripts/container-smoke.sh $(IMAGE)

# Rules only by default. EVAL_VALIDATE=1 adds sandbox runs; EVAL_PROVIDER=
# anthropic|openai_compatible|ollama adds the assisted and model engines
# (EVAL_MODEL, EVAL_BASE_URL, EVAL_API_KEY_ENV); EVAL_ONLY=slug,slug narrows.
eval:
	go test -tags eval -count=1 -run TestCorpus -v -timeout 60m ./internal/eval

# Regenerates THIRD_PARTY_LICENSES.md from the modules linked into the binary.
licenses: $(TOOLS)/go-licenses
	GOROOT=$(TOOLCHAIN_ROOT) $(TOOLS)/go-licenses report ./... --template scripts/third-party-licenses.tmpl $(LICENSE_IGNORE) > THIRD_PARTY_LICENSES.md

# Writes the CLI reference, docs/cli.md, from the command tree. docs/ stays
# out of the repository.
docs:
	go test -count=1 ./internal/cli -run TestCLIReferenceRenders -args -update-docs

# Runs the sandbox against the local Docker daemon (pulls small public images).
test-docker:
	COMPOSEWRIGHT_DOCKER_TESTS=1 go test -count=1 -run TestDocker -v -timeout 15m ./internal/sandbox

# Needs network: runs gen through a recording proxy and checks every contacted host.
egress:
	COMPOSEWRIGHT_NETWORK_TESTS=1 go test -count=1 -run TestEgress -v ./internal/cli

# Needs unprivileged user namespaces: gen on a local directory with no network.
offline: build
	scripts/offline-smoke.sh bin/composewright

# One command for the development container: build, then start or restart it
# on the new binary (docker-compose.dev.yml).
dev: build
	mkdir -p -m 700 .dev
	docker compose -f docker-compose.dev.yml up -d --force-recreate --remove-orphans

fmt: $(TOOLS)/golangci-lint
	$(TOOLS)/golangci-lint fmt ./...

vuln: $(TOOLS)/govulncheck
	$(TOOLS)/govulncheck ./...

clean:
	rm -rf bin/composewright

tools: $(TOOLS)/golangci-lint $(TOOLS)/go-licenses $(TOOLS)/govulncheck

$(TOOLS)/golangci-lint:
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(TOOLS)/go-licenses:
	GOBIN=$(TOOLS) go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)

$(TOOLS)/govulncheck:
	GOBIN=$(TOOLS) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
