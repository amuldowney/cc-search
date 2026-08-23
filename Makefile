# FTS5 is not compiled into go-sqlite3 by default, so every build and test
# target must carry the sqlite_fts5 tag.
TAGS := sqlite_fts5
PREFIX ?= $(HOME)/.local

.PHONY: build test test-clients generate-clients vet install clean

build:
	go build -tags $(TAGS) -o cc-search ./cmd/cc-search

test:
	go test -tags $(TAGS) ./...

test-clients: generate-clients
	python3 -m unittest discover -s clients/python -p 'test_*.py'
	npm --prefix clients/typescript test

generate-clients:
	node scripts/generate-clients.mjs

vet:
	go vet -tags $(TAGS) ./...

install: build
	install -Dm755 cc-search $(PREFIX)/bin/cc-search

clean:
	rm -f cc-search
