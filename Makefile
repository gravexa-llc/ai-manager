GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
CLAUDE  := $(shell command -v claude 2>/dev/null)
GITLEAKS := $(shell command -v gitleaks 2>/dev/null)

# Go files gofmt checks: everything except dot directories (.git, .claude)
# and testdata.
GOFILES = $(shell find . \( -path './.*' -o -name testdata \) -prune -o -name '*.go' -print)

.PHONY: check go-check fmt-check vet test race plugin-check secrets hooks build clean

## check: the CI suite. There is no GitHub Actions: scripts/ci.sh runs this on
## every PR on the maintainer's Mac. The credential scan when gitleaks is on
## PATH, gofmt, go vet, tests under the race detector, and the plugin checks
## when claude is on PATH (scripts/ci.sh requires both).
check: secrets fmt-check vet race plugin-check

## go-check: the Go part alone, without the race detector (quicker)
go-check: fmt-check vet test

fmt-check:
	@out="$$(gofmt -l $(GOFILES))" || exit 1; \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

## plugin-check: claude plugin validate (the plugin, and the marketplace at the
## repo root), and claude plugin test when the mod has tests
# `claude plugin test` takes the plugin's root (the folder holding
# hooks/hooks.json), not plugin/mod, and fails when there are no tests.
plugin-check:
ifeq ($(CLAUDE),)
	@echo "claude not on PATH: skipping plugin checks"
else
	claude plugin validate plugin/
	claude plugin validate .
	@if [ -n "$$(find plugin/mod -name '*.test.ts' -o -name '*.test.tsx')" ]; then \
		echo "claude plugin test plugin/"; claude plugin test plugin/; \
	else \
		echo "no tests in plugin/mod: skipping claude plugin test"; \
	fi
endif

## secrets: scan the working tree for credentials with gitleaks and
## .gitleaks.toml. It is first in check: it takes a second, and a credential
## is the failure to hear about before anything else runs.
secrets:
ifeq ($(GITLEAKS),)
	@echo "gitleaks not on PATH: skipping the credential scan (brew install gitleaks)"
else
	gitleaks dir . --config .gitleaks.toml --redact --verbose --no-banner --log-level warn
endif

## hooks: install the pre-commit hook (scripts/hooks/pre-commit), which
## refuses a commit whose staged changes hold a credential. It goes in the
## repo's shared hooks folder, so every worktree gets it. Run it once per
## clone, and again when the hook changes.
hooks:
	@sh scripts/hooks/install

## build: build aimd and aim into bin/
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

clean:
	rm -rf bin/
