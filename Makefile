BIN := bin/gophotocull

.PHONY: build test vet clean
build:
	go build -o $(BIN) ./cmd/gophotocull
test:
	go test ./...
vet:
	go vet ./...
clean:
	rm -rf bin
