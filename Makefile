VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= mise x -- go
GOFMT   ?= mise x -- gofmt
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all build test lint dist build-amd64 build-arm64 e2e e2e-releases e2e-clean clean

all: lint test build

build:
	$(BUILD) -o rpak ./cmd/rpak

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	@out="$$($(GOFMT) -l cmd internal)"; \
	if [ -n "$$out" ]; then echo "gofmt -l reports:"; echo "$$out"; exit 1; fi

build-amd64:
	GOOS=linux GOARCH=amd64 $(BUILD) -o dist/rpak-linux-amd64 ./cmd/rpak

build-arm64:
	GOOS=linux GOARCH=arm64 $(BUILD) -o dist/rpak-linux-arm64 ./cmd/rpak

# The same binaries again under `uname -m` names, so that one download
# command works on either architecture: rpak-linux-$(uname -m).
dist: build-amd64 build-arm64
	cp dist/rpak-linux-amd64 dist/rpak-linux-x86_64
	cp dist/rpak-linux-arm64 dist/rpak-linux-aarch64

# End-to-end test with the distribution's flatpak in a debian:trixie
# container (podman). The image and the runtime are cached (several hundred
# MB the first time); E2E_FRESH=1 starts from scratch, e2e-clean removes them.
e2e: build e2e-releases
	./test/e2e/run.sh

# rpak self-update's test: this rpak built as two releases, which the test
# serves as GitHub would.
e2e-releases:
	for v in v0.0.1 v0.0.2; do \
		CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$$v" -o test/e2e/out/self-update/$$v/rpak ./cmd/rpak || exit 1; \
	done

e2e-clean:
	./test/e2e/run.sh --clean

clean:
	rm -rf rpak dist
