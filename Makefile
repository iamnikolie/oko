.PHONY: build install uninstall test vet fmt clean e2e

BIN := oko
PREFIX ?= $(HOME)/.local
PKG := github.com/iamnikolie/oko/cmd

# Version stamped into the binary. Falls back to the short commit when the tree
# has no tag yet, so a local build is still identifiable.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG).version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/$(BIN) $(PREFIX)/bin/$(BIN)
	@echo "linked $(PREFIX)/bin/$(BIN) -> $(CURDIR)/$(BIN)"

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN)
	rm -rf dist

e2e:
	go test ./e2e/ -count=1
