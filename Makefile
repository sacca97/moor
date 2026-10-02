BIN    := moor
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: build install uninstall test dist release hooks clean

build:
	go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $(BIN) ./cmd/moor

install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 755 $(BIN) $(DESTDIR)$(BINDIR)/$(BIN)

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(BIN)

test:
	go test ./...

# Release archives, as fetched by install.sh.
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

dist:
	rm -rf dist && mkdir dist
	for p in $(PLATFORMS); do \
		GOOS=$${p%/*} GOARCH=$${p#*/} CGO_ENABLED=0 go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o dist/$(BIN) ./cmd/moor && \
		tar -C dist -czf dist/$(BIN)_$${p%/*}_$${p#*/}.tar.gz $(BIN) && rm dist/$(BIN) || exit 1; \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt
	# install.sh with this release's version and archive checksums baked in
	awk -v v="$(VERSION)" 'BEGIN { while ((getline l < "dist/checksums.txt") > 0) c = c (c ? "\n" : "") l } \
		{ gsub(/@VERSION@/, v); sub(/@CHECKSUMS@/, c) } 1' install.sh > dist/install.sh
	chmod 755 dist/install.sh

# Use the checks in .githooks before every commit.
hooks:
	git config core.hooksPath .githooks

# Tag and push a release: make release VERSION=0.1.0
# The pushed tag starts the GitHub workflow that builds and publishes it.
release:
	@case "$(origin VERSION)-$(VERSION)" in \
		command*-[0-9]*.[0-9]*.[0-9]*) ;; \
		*) echo "usage: make release VERSION=X.Y.Z" >&2; exit 1 ;; \
	esac
	@[ "$$(git rev-parse --abbrev-ref HEAD)" = main ] || { echo "release from main" >&2; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || { echo "working tree is not clean; commit first" >&2; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null || { echo "$(VERSION) already exists" >&2; exit 1; }
	@git fetch -q origin main && [ "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" ] || \
		{ echo "main differs from origin/main; push or pull first" >&2; exit 1; }
	$(MAKE) test
	git tag -a $(VERSION) -m "moor $(VERSION)"
	git push origin $(VERSION)
	@echo "pushed $(VERSION); watch the build: https://github.com/sacca97/moor/actions"

clean:
	rm -rf $(BIN) dist
