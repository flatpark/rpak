VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= mise x -- go
GOFMT   ?= mise x -- gofmt
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all build test lint dist build-amd64 build-arm64 e2e clean

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

dist: build-amd64 build-arm64

# End-to-end test with the distribution's flatpak in a debian:trixie
# container (podman); downloads several hundred MB.
e2e: build
	./test/e2e/run.sh

clean:
	rm -rf rpak dist
