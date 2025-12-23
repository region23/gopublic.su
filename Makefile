SERVER_ADDR ?= localhost:4443
DOMAIN_NAME ?= localhost

.PHONY: build-server build-client clean

build-server:
	go build -o bin/server cmd/server/main.go

# Build client with baked-in server address
build-client:
	@echo "Building client for Server: $(SERVER_ADDR)"
	go build -ldflags "-X main.ServerAddr=$(SERVER_ADDR)" -o bin/gopublic-client cmd/client/main.go

clean:
	rm -rf bin/
