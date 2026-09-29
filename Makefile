BIN     := bin/cull
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/jefflaplante/cull/internal/cli.version=$(VERSION)

.PHONY: build install test vet tidy clean
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/cull
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/cull
test:
	go test ./...
vet:
	go vet ./...
tidy:
	go mod tidy
clean:
	rm -rf bin
