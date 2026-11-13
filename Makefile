BINARY      ?= forge
VERSION     ?= dev
GO          ?= go
LDFLAGS     := -s -w -X main.version=$(VERSION)
BUILD_FLAGS := -trimpath -buildvcs=false

.PHONY: all build run dev test vet fmt clean install

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