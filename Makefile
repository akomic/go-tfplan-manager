.PHONY: build test vet fmt fmt-check release clean

BINARY = go-tfplan-manager
PLATFORMS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS = -ldflags "-s -w -X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildDate=$(BUILD_DATE)"

build:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o $(BINARY) .

test:
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "Run 'make fmt':"; gofmt -l .; exit 1)

# Build all release binaries into dist/ with a checksums.txt
release: clean
	mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "Building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath $(LDFLAGS) -o dist/$(BINARY)-$$os-$$arch . || exit 1; \
	done
	cd dist && sha256sum $(BINARY)-* > checksums.txt

clean:
	rm -rf dist $(BINARY)
