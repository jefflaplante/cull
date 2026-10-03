BIN     := bin/cull
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/jefflaplante/cull/internal/cli.version=$(VERSION)

.PHONY: build install test vet tidy clean dist
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
	rm -rf bin dist

# dist: a universal macOS binary (Apple silicon + Intel) packed for a GitHub release:
# dist/cull_macos_universal.tar.gz (cull, README.md, USAGE.md) and its .sha256; the name has
# no version, so releases/latest/download/cull_macos_universal.tar.gz always works.
# lipo needs macOS; the release workflow runs this on a macOS runner.
DIST := dist
PKG  := cull_macos_universal
dist:
	rm -rf $(DIST) && mkdir -p $(DIST)/$(PKG)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $(DIST)/cull-arm64 ./cmd/cull
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $(DIST)/cull-amd64 ./cmd/cull
	lipo -create -output $(DIST)/$(PKG)/cull $(DIST)/cull-arm64 $(DIST)/cull-amd64
	cp README.md USAGE.md $(DIST)/$(PKG)/
	tar -C $(DIST) -czf $(DIST)/$(PKG).tar.gz $(PKG)
	cd $(DIST) && shasum -a 256 $(PKG).tar.gz > $(PKG).tar.gz.sha256
	rm -f $(DIST)/cull-arm64 $(DIST)/cull-amd64
