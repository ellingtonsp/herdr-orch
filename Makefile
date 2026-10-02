VERSION ?= $(shell sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml | head -1)
LDFLAGS := -s -w -X github.com/ellingtonsp/herdr-orch/internal/daemon.Version=v$(VERSION)
GOFLAGS := -trimpath
OUT ?= bin

.PHONY: build test race vet install-cli link integration live clean

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/herdr-orch ./cmd/herdr-orch
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/horch ./cmd/horch

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race -count=1 ./...

# Symlinks horch into ~/.local/bin (or $HORCH_BIN_DIR). Never replaces a regular file.
install-cli: build
	$(OUT)/herdr-orch install-cli

# Local development: register this checkout as the plugin (link skips [[build]]).
link: build
	herdr plugin link $(CURDIR)

# Exit scenarios against an isolated herdr session (fake agents, plus real claude/pi/codex
# start-and-release unless SKIP_REAL_AGENTS=1).
integration: build
	./scripts/integration.sh

# Real claude + codex workers each do a small task end to end (spends a few agent turns).
live: build
	./scripts/live-dispatch.sh

clean:
	rm -rf bin dist
