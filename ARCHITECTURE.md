# GoPublic Implementation Walkthrough

## Overview
GoPublic is a self-hosted reverse proxy service.
This walkthrough covers the **Server** (Control Plane, Ingress, Dashboard) and the **Client** (CLI, Tunnel).

## Server Components

### 1. Dashboard & Authentication
- **URL**: `app.DOMAIN_NAME`
- **Auth**: Telegram Login Widget.
- **Features**: Displays Auth Token and assigned domains.
- **Implementation**: `internal/dashboard`

### 2. Public Ingress
- **URL**: `*.DOMAIN_NAME`
- **TLS**: Automatic Let's Encrypt certificates.
- **Routing**: Routes subdomains to active Yamux sessions.
- **Implementation**: `internal/ingress`

### 3. Control Plane
- **Port**: `:4443`
- **Protocol**: Secure TCP (TLS).
- **Logic**: Handles Client Handshake, Auth, and Multiplexing.
- **Implementation**: `internal/server`

## Client Components

### 1. CLI (`cmd/client`)
- Built with `spf13/cobra`.
- **Reference**:
  - `gopublic auth <token>`: Saves token to `~/.gopublic`.
  - `gopublic start <port>`: Starts tunneling `localhost:<port>`.

### 2. Tunnel Logic (`internal/client/tunnel`)
- Connects to Server via TLS.
- Multiplexes connections with `yamux`.
- Proxies requests to local port.

### 3. Inspector (`internal/client/inspector`)
- **Port**: `:4040`
- **UI**: Embedded Web Interface displaying real-time HTTP requests.
- **Implementation**: Captures `http.Request` objects during proxying, stores them in an in-memory ring buffer (last 100), and serves them via an internal API.

## Testing Locally (Dev Mode)
To run the entire system on a local machine with 127.0.0.1:
1. **Server**: Use `INSECURE_HTTP=true` and `DOMAIN_NAME=127.0.0.1` in `.env`. Run with `sudo` to bind port 80.
2. **Client**: Connect to `localhost:4443`.
3. **Login**: Use the Telegram widget at `http://127.0.0.1/login`. callback will be `http://127.0.0.1/auth/telegram`.

## Deployment

### Server
1. Create `.env`:
   ```ini
   DOMAIN_NAME=example.com
   EMAIL=admin@example.com
   TELEGRAM_BOT_TOKEN=...
   TELEGRAM_BOT_NAME=...
   ```
2. Run with Docker:
   ```bash
   docker-compose up -d --build
   ```

### Client
1. Build (replace SERVER_ADDR):
   ```bash
   make build-client SERVER_ADDR=example.com:4443
   ```
2. Authenticate:
   ```bash
   ./bin/gopublic-client auth sk_live_...
   ```
3. Start Tunnel:
   ```bash
   ./bin/gopublic-client start 3000
   ```
