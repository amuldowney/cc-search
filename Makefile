# FTS5 is not compiled into go-sqlite3 by default, so every build and test
# target must carry the sqlite_fts5 tag.
TAGS := sqlite_fts5
PREFIX ?= $(HOME)/.local
INSTALL_PATH ?= $(PREFIX)/bin/cc-search

.PHONY: build test vet install verify-install deploy clean

build:
	go build -tags $(TAGS) -o cc-search ./cmd/cc-search

test:
	go test -tags $(TAGS) ./...

vet:
	go vet -tags $(TAGS) ./...

# ~/.local/bin/cc-search is a symlink to the persistent Projects toolchain in
# the devbox. Resolve that link before installing so the link survives a
# container restart; write a temporary file and rename it to avoid a partial
# executable being observed by another session.
install: build
	@set -eu; \
	dest="$(INSTALL_PATH)"; \
	while [ -L "$$dest" ]; do \
		target="$$(readlink "$$dest")"; \
		case "$$target" in \
			/*) dest="$$target" ;; \
			*) dest="$$(dirname "$$dest")/$$target" ;; \
		esac; \
	done; \
	dir="$$(dirname "$$dest")"; \
	mkdir -p "$$dir"; \
	tmp="$$(mktemp "$$dir/.cc-search.XXXXXX")"; \
	trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
	install -m755 cc-search "$$tmp"; \
	mv -f "$$tmp" "$$dest"; \
	trap - EXIT HUP INT TERM; \
	printf 'installed %s\n' "$$dest"

verify-install:
	@set -eu; \
	dest="$(INSTALL_PATH)"; \
	while [ -L "$$dest" ]; do \
		target="$$(readlink "$$dest")"; \
		case "$$target" in \
			/*) dest="$$target" ;; \
			*) dest="$$(dirname "$$dest")/$$target" ;; \
		esac; \
	done; \
	test -x "$$dest"; \
	cmp -s cc-search "$$dest" || { echo "installed binary differs from ./cc-search: $$dest" >&2; exit 1; }; \
	printf 'verified %s\n' "$$dest"

# The supported deployment entry point. It validates the source, builds with
# the mandatory FTS5 tag, installs atomically, and verifies the exact bytes.
deploy: test vet
	@$(MAKE) --no-print-directory install INSTALL_PATH="$(INSTALL_PATH)"
	@$(MAKE) --no-print-directory verify-install INSTALL_PATH="$(INSTALL_PATH)"

clean:
	rm -f cc-search
