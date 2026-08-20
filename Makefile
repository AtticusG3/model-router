# model-router — local verification targets (no hosted CI).
#
# The Go toolchain is portable at ~/go-toolchain (no system install); put it on
# PATH first, same as the README build instructions:
#
#   export PATH=$$HOME/go-toolchain/go/bin:$$PATH
#
# `make check` is the CI gate: formatting, vet, and tests.

GO    ?= go
GOFMT ?= gofmt

.PHONY: fmt-check vet test check build

# fmt-check fails if any .go file is not gofmt-formatted.
fmt-check:
	@out=$$($(GOFMT) -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$out"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

check: fmt-check vet test

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o build/model-router ./cmd/model-router
