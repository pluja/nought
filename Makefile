# Developer tasks. `make check` is what CI runs.
.PHONY: build test cover lint vuln fmt check dist

GO ?= go
COVER_MIN ?= 80

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o nou ./cmd/nou

test:
	$(GO) test -race ./...

cover:
	$(GO) test -race -coverpkg=./internal/... -coverprofile=cover.out ./...
	@$(GO) tool cover -func=cover.out | tail -1
	@$(GO) tool cover -func=cover.out | awk '/^total:/ { sub("%","",$$3); if ($$3+0 < $(COVER_MIN)) { print "coverage below $(COVER_MIN)%"; exit 1 } }'

lint:
	$(GO) vet ./...
	$(GO) tool staticcheck ./...
	golangci-lint run

vuln:
	$(GO) tool govulncheck ./...

fmt:
	golangci-lint fmt

check: lint cover vuln build

# Release archives for every system, into dist/: make dist VERSION=v0.1.0
dist:
	tools/dist.sh $(VERSION)

# --- Local test environment (never touches your real nought data) ---------
# A Dovecot server on 127.0.0.1:10993 (TLS) and :10143 (STARTTLS), and a
# NOUGHT_HOME inside the repository. See testenv/README.md.
.PHONY: testenv-up testenv-down testenv-reset testenv-seed dev integration screenshots demo

TESTENV   := podman compose -p nought-testenv -f testenv/compose.yaml
DEV_ENV   := NOU_DEV=1 NOUGHT_HOME=$(CURDIR)/testenv/home NOU_DEV_CA=$(CURDIR)/testenv/certs/tls.crt

testenv-up:
	testenv/gen-cert.sh
	$(TESTENV) up -d --wait

testenv-down:
	$(TESTENV) down

# Removes the test server's mail and the dev NOUGHT_HOME.
testenv-reset:
	$(TESTENV) down -v
	rm -rf testenv/home

testenv-seed:
	$(DEV_ENV) $(GO) run ./testenv/seed

# Runs nou against the test environment: make dev ARGS="sync --dry-run"
dev:
	CGO_ENABLED=0 $(GO) build -trimpath -o testenv/nou ./cmd/nou
	$(DEV_ENV) testenv/nou $(ARGS)

integration:
	$(DEV_ENV) $(GO) test -tags integration -count=1 ./internal/integration/...

# README screenshots, from a made-up mailbox (no real mail): ANSI screens
# from the TUI, drawn as SVG terminal windows.
screenshots:
	rm -rf $(CURDIR)/.screens && mkdir -p $(CURDIR)/.screens docs/images
	TZ=UTC $(GO) test ./internal/tui -run TestScreenshots -count=1 -screenshots=$(CURDIR)/.screens
	for f in .screens/*.ansi; do \
		$(GO) run ./tools/ansisvg -title "nou" < $$f > docs/images/$$(basename $$f .ansi).svg || exit 1; \
	done
	rm -rf $(CURDIR)/.screens

# The README demo GIF: the real nou over a made-up mailbox, in the test
# environment only (make testenv-up first; needs vhs).
demo:
	testenv/demo.sh
