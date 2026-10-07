VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test run release clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/agg ./cmd/agg

test:
	go vet ./...
	go test ./...
	node --test 'web/sdk/*.test.mjs'

run:
	go run ./cmd/agg serve

# Static binaries for the common platforms (pure Go, no cgo).
release:
	@for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "dist/agg-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/agg-$$os-$$arch$$ext ./cmd/agg; \
	done

clean:
	rm -rf dist
