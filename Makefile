export GOMAXPROCS := 2
export GOFLAGS := -p=1
export GOMEMLIMIT := 1GiB
TRPLUGIN ?= go tool trplugin
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
.NOTPARALLEL:
.PHONY: test frontend build pack
test:
	go test ./...
	go vet ./...
frontend:
	pnpm --dir web install --frozen-lockfile
	pnpm --dir web build
build: frontend
	@set -eu; for platform in $(PLATFORMS); do \
	 os=$${platform%/*}; arch=$${platform#*/}; suffix=; \
	 if [ "$$os" = windows ]; then suffix=.exe; fi; \
	 mkdir -p "dist/$$platform/bin"; \
	 CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" -o "dist/$$platform/bin/plugin$$suffix" ./cmd/plugin; \
	done
pack: build
	$(TRPLUGIN) pack --manifest plugin.json --frontend dist/frontend --bin-dir dist --out release
	$(TRPLUGIN) validate release/*.tar.gz
