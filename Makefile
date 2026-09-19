BINARY := lazytmux
# The embedded version keeps git describe's full suffix so the updater and
# diagnostics see the real build state; the Version panel strips it for
# display (update.ReleaseVersion).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
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
