BINARY  := mirocraft
PKG     := github.com/collybia/mirocraft
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

export CGO_ENABLED := 0

.PHONY: build build-all test test-race lint fmt tidy clean dev web web-test e2e

# The Go binary embeds web/dist, so a release build makes the panel first.
web:
	cd web && npm ci && npm run build

web-test:
	cd web && npm test

# End-to-end run against a live daemon. Pass the credentials the daemon
# printed on its first start.
#   make e2e URL=http://127.0.0.1:8080 EMAIL=admin@localhost PASS=...
e2e:
	cd web && node scripts/e2e.mjs $(URL) $(EMAIL) $(PASS) ../.e2e-shots

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

build-all:
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64   ./cmd/$(BINARY)
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64   ./cmd/$(BINARY)
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-windows-amd64.exe ./cmd/$(BINARY)

test:
	go test ./...

# The race detector needs cgo and a C compiler, so this is the CI target
# rather than the default one; the concurrency in internal/runner is the
# reason it must run somewhere.
test-race:
	CGO_ENABLED=1 go test -race ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf bin/

dev:
	go run ./cmd/$(BINARY) --log-level=debug
