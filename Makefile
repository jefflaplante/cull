BIN     := bin/gophotocull
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/jefflaplante/gophotocull/internal/cli.version=$(VERSION)

.PHONY: build install test vet tidy clean
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/gophotocull
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/gophotocull
test:
	go test ./...
vet:
	go vet ./...
tidy:
	go mod tidy
clean:
	rm -rf bin
