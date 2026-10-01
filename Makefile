BIN    := moor
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: build install uninstall test clean

build:
	go build -o $(BIN) ./cmd/moor

install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 755 $(BIN) $(DESTDIR)$(BINDIR)/$(BIN)

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(BIN)

test:
	go test ./...

clean:
	rm -f $(BIN)
