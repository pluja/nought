# Developer tasks. `make check` is what CI runs.
.PHONY: build test cover lint vuln fmt check

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
