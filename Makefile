.PHONY: build install uninstall test vet fmt lint clean e2e skill

BIN := oko
PREFIX ?= $(HOME)/.local
# Local agent-config installer (trafaret over the personal agentic-os config) that fans
# cmd/skill.md out to every harness's skills dir. Absent on other machines —
# `make skill` then does nothing.
AO ?= trafaret --config $(HOME)/development/iamnikolie/agentic-os
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
	@$(MAKE) --no-print-directory skill

# Copy the embedded agent reference into agentic-os and fan it out to the
# Claude, Codex and Cursor skill dirs.
skill:
	@root=$$($(AO) root 2>/dev/null); \
	if [ -n "$$root" ] && [ -d "$$root/global/skills" ]; then \
		mkdir -p "$$root/global/skills/oko" && \
		cp cmd/skill.md "$$root/global/skills/oko/SKILL.md" && \
		for h in claude-personal codex cursor; do $(AO) install $$h | head -1; done; \
	else echo "skill: $(AO) not found, skipping"; fi

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# gofmt must have nothing to change, and go vet must pass.
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

clean:
	rm -f $(BIN)
	rm -rf dist

e2e:
	go test ./e2e/ -count=1
