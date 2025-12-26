SERVER_ADDR ?= localhost:4443
DOMAIN_NAME ?= localhost
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: build-server build-client clean docker-build docker-up

build-server:
	go build -ldflags "-X gopublic/internal/version.Version=$(VERSION)" -o bin/server cmd/server/main.go

# Build client with baked-in server address
build-client:
	@echo "Building client for Server: $(SERVER_ADDR)"
	go build -ldflags "-X main.ServerAddr=$(SERVER_ADDR)" -o bin/gopublic-client cmd/client/main.go

clean:
	rm -rf bin/

# Docker commands with automatic version from git tag
docker-build:
	VERSION=$(VERSION) docker-compose build

docker-up:
	VERSION=$(VERSION) docker-compose up -d --build
