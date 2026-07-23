APP := orphaned-pvc-scanner
BIN_DIR := bin
BIN := $(BIN_DIR)/$(APP)
GOCACHE ?= /tmp/$(APP)-gocache
GOFLAGS ?=

.PHONY: all fmt test build run clean help

all: fmt test build

fmt:
	gofmt -w main.go main_test.go

test:
	GOCACHE=$(GOCACHE) go test $(GOFLAGS) ./...

build:
	mkdir -p $(BIN_DIR)
	GOCACHE=$(GOCACHE) go build $(GOFLAGS) -o $(BIN) .

run:
	GOCACHE=$(GOCACHE) go run $(GOFLAGS) . $(ARGS)

clean:
	rm -rf $(BIN_DIR)

help:
	@echo "Targets:"
	@echo "  make fmt          Format Go files"
	@echo "  make test         Run tests"
	@echo "  make build        Build $(BIN)"
	@echo "  make run ARGS=... Run the scanner"
	@echo "  make clean        Remove build outputs"
	@echo "  make all          Format, test, and build"
