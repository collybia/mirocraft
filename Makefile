BINARY  := mirocraft
PKG     := github.com/collybia/mirocraft
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

export CGO_ENABLED := 0

.PHONY: build build-all test test-race test-linux test-live lint fmt tidy clean dev web web-test e2e

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

# Runs the suite the way CI does: on Linux, with the race detector. Worth a
# target of its own because the differences are real and silent from a Windows
# machine — a backslash is a path separator on one host and an ordinary
# character in a filename on the other, and a process killed on Linux lingers
# as a zombie until something reaps it. Both of those shipped red builds
# before this existed. Needs Docker.
test-linux:
	docker run --rm -v "$(CURDIR)":/src -w /src -e CGO_ENABLED=1 golang:1.26 go test -race -timeout 15m -count=1 ./...

# Checks the core providers against the real Mojang and PaperMC APIs, jar
# download included. Slow and network-dependent, so it is not part of `test`,
# but run it before a release: upstream changes its API without asking, and
# fixtures cannot notice.
test-live:
	go test -tags live -run TestLive -v ./internal/core/

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
