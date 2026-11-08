GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
CLAUDE  := $(shell command -v claude 2>/dev/null)

# Go files gofmt checks: everything except dot directories (.git, .claude)
# and testdata.
GOFILES = $(shell find . \( -path './.*' -o -name testdata \) -prune -o -name '*.go' -print)

.PHONY: check go-check fmt-check vet test race plugin-check build clean

## check: the CI suite. There is no GitHub Actions: scripts/ci.sh runs this on
## every PR on the maintainer's Mac. gofmt, go vet, tests under the race
## detector, and the plugin checks when claude is on PATH (scripts/ci.sh
## requires it).
check: fmt-check vet race plugin-check

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

## build: build aimd and aim into bin/
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

clean:
	rm -rf bin/
