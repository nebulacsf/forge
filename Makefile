BINARY      ?= forge
# The build stamp comes from git describe so `--version` reports the release tag
# plus the extra commits and the dirty flag when the tree is not pristine:
# v1.2.3, v1.2.3-4-gabc1234, v1.2.3-4-gabc1234-dirty. No git, or no tag, falls
# back to the short commit and finally to "dev". Override with `make VERSION=...`.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO          ?= go
LDFLAGS     := -s -w -X main.version=$(VERSION)
BUILD_FLAGS := -trimpath -buildvcs=false

.PHONY: all build run dev test vet fmt clean install vendor

all: build

build:
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

run: build
	./$(BINARY)

dev:
	CGO_ENABLED=0 $(GO) run . --no-browser

test:
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

fmt:
	$(GO) fmt ./...

install:
	CGO_ENABLED=0 $(GO) install $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" .

clean:
	rm -f $(BINARY)

# Re-download the pinned frontend libraries under web/vendor. Only needed when
# bumping a version in scripts/vendor.sh; the files are committed for offline
# builds.
vendor:
	./scripts/vendor.sh
