BINARY := renderroute
PREFIX ?= $(HOME)/.local
BINDIR := $(PREFIX)/bin
SHAREDIR := $(PREFIX)/share/renderroute

.PHONY: all build test clean install install-dms uninstall

all: build

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/renderroute

test:
	go test -v -race ./...

install: build
	install -Dm755 $(BINARY) $(BINDIR)/$(BINARY)
	mkdir -p $(SHAREDIR)
	rm -rf $(SHAREDIR)/dms-plugin
	cp -r dms-plugin $(SHAREDIR)/dms-plugin

install-dms: install
	$(BINDIR)/$(BINARY) dms install --source $(SHAREDIR)/dms-plugin

uninstall:
	-$(BINDIR)/$(BINARY) dms uninstall
	rm -f $(BINDIR)/$(BINARY)
	rm -rf $(SHAREDIR)

clean:
	rm -f $(BINARY)
