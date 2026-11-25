STATICCHECK_VERSION := v0.8.2
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/runtime-bridge/rbridge/internal/buildinfo.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build fakeagent test race cover vet fmt fmt-check staticcheck lint licenses cross bench compat clean

all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/rbridge ./cmd/rbridge

fakeagent:
	go build -o bin/rbridge-fakeagent ./internal/tools/fakeagent

test:
	go test ./...

race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

lint: fmt-check vet staticcheck

licenses:
	./scripts/check-licenses.sh

cross:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o bin/rbridge-$$os-$$arch$$ext ./cmd/rbridge || exit 1; \
	done

bench:
	go test -run '^$$' -bench . -benchtime 2s ./internal/service/ ./internal/skills/spec/ ./internal/providers/acp/

compat:
	RBRIDGE_IT_REPORT=$(CURDIR)/docs/compatibility-run.md go test -tags integration -count=1 -v -timeout 60m ./tests/integration/

clean:
	rm -rf bin coverage.out
