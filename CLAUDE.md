# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

GoPublic is a self-hosted ngrok clone — a reverse proxy that exposes local services to the public internet via secure tunnels. Written in Go.

## Build Commands

```bash
# Build server
make build-server

# Build client (with server address baked in)
make build-client SERVER_ADDR=localhost:4443

# Run tests
go test ./...

# Run single test
go test -v -run TestName ./path/to/package

# Build all packages (verify compilation)
go build ./...
```

## Running Locally

```bash
# Server (creates .env with DOMAIN_NAME=localhost, INSECURE_HTTP=true)
go run cmd/server/main.go
# Listens on :8080 (HTTP Ingress) and :4443 (TCP Control)

# Client
./bin/gopublic-client auth sk_live_12345  # seed token for local dev
./bin/gopublic-client start 3000

# Test tunnel manually
curl -H "Host: misty-river" http://localhost:8080/

# Inspector UI
open http://localhost:4040
```

## Architecture

**Two binaries:**
- `cmd/server/main.go` — Public-facing server (HTTP ingress + control plane)
- `cmd/client/main.go` — CLI client for tunneling

**Server components (`internal/`):**
- `server/` — Control plane on `:4443`, yamux multiplexing, handshake protocol
- `ingress/` — HTTP router (Gin), routes by Host header to dashboard or tunnels
- `dashboard/` — Telegram OAuth, user registration, token display
- `storage/` — SQLite via GORM (users, tokens, domains)
- `auth/` — Token generation (crypto/rand), session management (securecookie)
- `middleware/` — CSRF protection

**Client components (`internal/client/`):**
- `cli/` — Cobra commands: `auth`, `start`
- `tunnel/` — Yamux connection, reconnection with exponential backoff
- `config/` — User config (`~/.gopublic`) and project config (`gopublic.yaml`)
- `inspector/` — Local web UI on `:4040` for request inspection and replay

**Protocol (`pkg/protocol/`):**
- JSON messages: `AuthRequest`, `TunnelRequest`, `InitResponse`

## Key Patterns

**Tunnel flow:**
1. Client connects to server:4443 (TLS in prod, plain TCP locally)
2. Yamux session established
3. Handshake: AuthRequest → TunnelRequest → InitResponse
4. Server accepts HTTP, opens yamux stream to client
5. Client proxies stream to localhost:port

**Multi-tenant routing:**
- Root domain → landing page
- `app.{domain}` → dashboard
- `*.{domain}` → tunnel lookup by hostname

**Reconnection:** Exponential backoff 1s → 60s max, context-aware shutdown.

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `DOMAIN_NAME` | Root domain (enables HTTPS if set) |
| `PROJECT_NAME` | Branding name for landing page (default: "Go Public") |
| `EMAIL` | For Let's Encrypt registration (required if DOMAIN_NAME set) |
| `INSECURE_HTTP` | Set `true` for local dev without TLS |
| `TELEGRAM_BOT_TOKEN` | For OAuth |
| `TELEGRAM_BOT_NAME` | Bot username for login widget |
| `SESSION_HASH_KEY` | 32-byte hex for cookie signing (optional) |
| `SESSION_BLOCK_KEY` | 32-byte hex for cookie encryption (optional) |

## Database

SQLite with GORM auto-migration. Tables: `users`, `tokens`, `domains`.
Tokens stored as SHA256 hash, shown to user only once at creation.

## Ports

| Port | Purpose |
|------|---------|
| `:80` | HTTP & ACME challenges (prod) |
| `:443` | HTTPS Ingress (prod) |
| `:8080` | HTTP Ingress (dev mode) |
| `:4443` | Control Plane (TCP/TLS) |
| `:4040` | Inspector UI (client-side) |
