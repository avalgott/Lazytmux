BINARY := lazytmux
# The installed version is the latest release tag, not a commit-description:
# the dashboard Version panel should say v0.2.0, not v0.2.0-119-g...-dirty.
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

PREFIX ?= $(HOME)/.local

.PHONY: build test install uninstall update clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test -race -cover ./...

install: build
	install -d $(PREFIX)/bin
	install -m 755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)

update:
	git pull && $(MAKE) install

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

clean:
	rm -rf bin/
