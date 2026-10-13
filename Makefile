GO ?= go
BINARY ?= patchtree

.PHONY: build install run test race vet fmt fmt-check check clean

build:
	$(GO) build -trimpath -o $(BINARY) .

install:
	$(GO) install .

run:
	$(GO) run .

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w *.go

fmt-check:
	@test -z "$$(gofmt -l *.go)" || (echo 'Run make fmt'; exit 1)

check: fmt-check vet test

clean:
	rm -f patchtree patchtree.exe
