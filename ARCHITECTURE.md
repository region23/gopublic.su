# Server Implementation Walkthrough

## Overview
I have implemented the core Server component of `gopublic` with support for HTTPS, Automatic Routing, and Client Builds.

## Components Implemented

### 1. Domain Routing (`internal/ingress`)
The Ingress listener smartly routes traffic based on the `Host` header:
- **`DOMAIN_NAME`** (e.g. `example.com`): Serves the **Landing Page** (currently a stub).
- **`app.DOMAIN_NAME`** (e.g. `app.example.com`): Serves the **Dashboard** (currently a stub).
- **`*.DOMAIN_NAME`** (e.g. `foo.example.com`): Routes to the active **User Tunnel**.

### 2. Client Build System (`Makefile`)
The client binary needs to know where the server is. Instead of config files, we bake the address in at build time.
- **Variable**: `main.ServerAddr` in `cmd/client/main.go`.
- **Injection**: The `Makefile` uses `go build -ldflags "-X main.ServerAddr=..."` to set this variable.
- **Command**: `make build-client SERVER_ADDR=your-vps.com:4443`

### 3. HTTPS
- Integrated `autocert` for automatic Let's Encrypt certificates.
- Supports On-Demand TLS for all subdomains.

### 4. Database Layer
- SQLite storage for users/tokens.
- `gopublic.db` is persisted in `./data` via Docker volumes.

## Usage

### Server
Deploy with Docker:
```bash
# Configure .env
DOMAIN_NAME=example.com
EMAIL=admin@example.com

# Run
docker-compose up -d --build
```

### Client (Mock/Dev)
Build the client for your server:
```bash
make build-client SERVER_ADDR=example.com:4443
```
Running the client:
```bash
./bin/gopublic-client
```
The client will connect, authenticate, and listen for requests.
