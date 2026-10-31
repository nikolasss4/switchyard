GO ?= go
BIN := bin

.PHONY: build test race lint integration conformance bench demo clean

build:
	$(GO) build -o $(BIN)/ ./cmd/...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

lint:
	golangci-lint run

# Needs Docker (testcontainers), or TEST_POSTGRES_URL / TEST_REDIS_URL pointing at throwaway instances.
integration:
	$(GO) test -tags integration -count=1 ./...

clean:
	rm -rf $(BIN)
