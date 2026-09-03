BINARY := pgrun
VERSION ?= dev

.PHONY: build test vet fmt clean tidy install

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/pgrundev/pgrun-cli/internal/cli.Version=$(VERSION)" -o bin/$(BINARY) ./cmd/pgrun

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

tidy:
	go mod tidy

install: build
	install -m 0755 bin/$(BINARY) $(or $(PGRUN_INSTALL_DIR),/usr/local/bin)/$(BINARY)

clean:
	rm -rf bin dist
