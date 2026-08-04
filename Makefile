.PHONY: all build doubletake doubletake-ctl doubletake-ui doubletake-release doubletake-ctl-release manpages-release install install-man install-ui install-desktop uninstall test clean

PREFIX ?= /usr/local
MANDIR ?= $(PREFIX)/share/man
DATADIR ?= $(PREFIX)/share

all: doubletake doubletake-ctl doubletake-ui

build: all

doubletake:
	go build -o bin/doubletake ./cmd/doubletake

doubletake-ctl:
	go build -o bin/doubletake-ctl ./cmd/doubletake-ctl

doubletake-ui:
	go build -o bin/doubletake-ui ./cmd/doubletake-ui

doubletake-release:
	CGO_ENABLED=0 go build -ldflags='-s -w -extldflags=-static' -o doubletake ./cmd/doubletake

doubletake-ctl-release:
	CGO_ENABLED=0 go build -ldflags='-s -w -extldflags=-static' -o doubletake-ctl ./cmd/doubletake-ctl

manpages-release:
	tar -czf doubletake-manpages.tar.gz -C man man1

test:
	go test ./...

install: all install-man install-ui install-desktop
	install -d $(PREFIX)/bin
	install -m 755 bin/doubletake $(PREFIX)/bin/
	install -m 755 bin/doubletake-ctl $(PREFIX)/bin/
	install -m 755 bin/doubletake-ui $(PREFIX)/bin/
	sed 's|@PREFIX@|$(PREFIX)|g' packaging/doubletake-launch > $(PREFIX)/bin/doubletake-launch
	chmod 755 $(PREFIX)/bin/doubletake-launch

install-man:
	install -d $(MANDIR)/man1
	install -m 644 man/man1/doubletake.1 $(MANDIR)/man1/
	install -m 644 man/man1/doubletake-ctl.1 $(MANDIR)/man1/

install-ui:
	install -d $(DATADIR)/doubletake/ui
	cp -R ui/dist/client/. $(DATADIR)/doubletake/ui/

install-desktop:
	install -d $(DATADIR)/applications
	sed 's|@PREFIX@|$(PREFIX)|g' packaging/doubletake.desktop > $(DATADIR)/applications/doubletake.desktop
	sed 's|@PREFIX@|$(PREFIX)|g' packaging/doubletake.desktop > $(DATADIR)/applications/doubletake-shell.desktop

uninstall:
	rm -f $(PREFIX)/bin/doubletake
	rm -f $(PREFIX)/bin/doubletake-ctl
	rm -f $(PREFIX)/bin/doubletake-ui
	rm -f $(PREFIX)/bin/doubletake-launch
	rm -f $(MANDIR)/man1/doubletake.1
	rm -f $(MANDIR)/man1/doubletake-ctl.1
	rm -rf $(DATADIR)/doubletake/ui
	rm -f $(DATADIR)/applications/doubletake-shell.desktop

clean:
	rm -rf bin/
	go clean -testcache
