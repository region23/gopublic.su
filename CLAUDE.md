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

# Clean build artifacts
make clean
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
- `ingress/` — HTTP router (Gin), routes by Host header to dashboard or tunnels, bandwidth limiting
- `dashboard/` — Telegram/Yandex OAuth, user registration, token display, Terms of Service, Abuse reporting
- `storage/` — SQLite via GORM (users, tokens, domains, abuse_reports, user_bandwidths)
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

**Security:**
- Session cookies: HMAC-SHA256 signing + AES encryption (gorilla/securecookie)
- Tokens: SHA256 hashed in DB, plaintext shown only once at creation
- CSRF: Double-submit cookie pattern for POST endpoints
- Terms of Service acceptance required before using tunnels
- Abuse reporting with admin Telegram notifications

**Abuse Protection:**
- Daily bandwidth limit per user (default: 100MB)
- Terms of Service with explicit prohibition of malware/phishing
- Abuse report form with Telegram notifications to admin
- Domain limits per user (configurable)

## Environment Variables

### Core Settings

| Variable | Purpose | Default |
|----------|---------|---------|
| `DOMAIN_NAME` | Root domain (enables HTTPS if set) | *empty* (HTTP mode) |
| `PROJECT_NAME` | Branding name for landing page | `Go Public` |
| `EMAIL` | For Let's Encrypt registration | *required if DOMAIN_NAME set* |
| `INSECURE_HTTP` | Set `true` for local dev without TLS | `false` |
| `DB_PATH` | SQLite database file path | `gopublic.db` |
| `CONTROL_PLANE_PORT` | Control plane TCP port | `:4443` |
| `GITHUB_REPO` | GitHub repo for client downloads (e.g., `username/gopublic`) | *empty* |

### User Limits

| Variable | Purpose | Default |
|----------|---------|---------|
| `DOMAINS_PER_USER` | Number of domains assigned to new users | `2` |
| `DAILY_BANDWIDTH_LIMIT_MB` | Daily bandwidth limit per user in MB (0 = unlimited) | `100` |

### Authentication

| Variable | Purpose | Default |
|----------|---------|---------|
| `TELEGRAM_BOT_TOKEN` | Telegram Bot API token for OAuth | *empty* |
| `TELEGRAM_BOT_NAME` | Telegram bot username (without @) | *empty* |
| `YANDEX_CLIENT_ID` | Yandex OAuth application client ID | *empty* |
| `YANDEX_CLIENT_SECRET` | Yandex OAuth application client secret | *empty* |
| `SESSION_HASH_KEY` | 32-byte hex for cookie signing | *random in dev* |
| `SESSION_BLOCK_KEY` | 32-byte hex for cookie encryption | *random in dev* |

### Notifications & Admin

| Variable | Purpose | Default |
|----------|---------|---------|
| `ADMIN_TELEGRAM_ID` | Telegram user ID for abuse report notifications | *empty* |

### Error Tracking

| Variable | Purpose | Default |
|----------|---------|---------|
| `SENTRY_DSN` | Sentry DSN for error tracking | *empty* |
| `SENTRY_ENVIRONMENT` | Environment name (production, staging, development) | `development` |
| `SENTRY_SAMPLE_RATE` | Error sample rate (0.0 - 1.0) | `1.0` |

## Database

SQLite with GORM auto-migration. Tables:
- `users` — User accounts with Telegram/Yandex IDs, terms acceptance
- `tokens` — Auth tokens (SHA256 hashed)
- `domains` — User-assigned subdomains
- `abuse_reports` — Abuse reports from users
- `user_bandwidths` — Daily bandwidth usage tracking

## Dashboard Routes

| Path | Purpose |
|------|---------|
| `/` | Main dashboard (requires auth) |
| `/login` | Login page (Telegram + Yandex OAuth) |
| `/logout` | Logout |
| `/terms` | Terms of Service page |
| `/abuse` | Abuse report form |
| `/auth/telegram` | Telegram OAuth callback |
| `/auth/yandex` | Yandex OAuth initiation |
| `/auth/yandex/callback` | Yandex OAuth callback |
| `/link/telegram` | Link Telegram to existing account |
| `/api/regenerate-token` | POST: Regenerate auth token |
| `/api/accept-terms` | POST: Accept Terms of Service |

## Ports

| Port | Purpose |
|------|---------|
| `:80` | HTTP & ACME challenges (prod) |
| `:443` | HTTPS Ingress (prod) |
| `:8080` | HTTP Ingress (dev mode) |
| `:4443` | Control Plane (TCP/TLS) |
| `:4040` | Inspector UI (client-side) |

## Docker Deployment

```bash
# Production deployment
docker-compose up -d --build

# View logs
docker-compose logs -f
```

Requires `.env` with `DOMAIN_NAME`, `EMAIL`, and at least one OAuth provider configured.

## Key Files

| File | Purpose |
|------|---------|
| `internal/ingress/ingress.go` | HTTP routing, bandwidth limiting, tunnel proxying |
| `internal/dashboard/handler.go` | OAuth handlers, Terms/Abuse endpoints |
| `internal/server/registry.go` | Tunnel session registry with user tracking |
| `internal/storage/db.go` | Database operations |
| `internal/config/config.go` | Configuration loading from ENV |
| `.env.example` | Example environment configuration |
