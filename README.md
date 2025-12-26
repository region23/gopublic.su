# GoPublic

GoPublic is a self-hosted reverse proxy service (similar to ngrok) that allows you to expose local services to the public internet via a secure tunnel.

## Configuration

You can configure the server using **Environment Variables** or a **`.env`** file placed in the same directory as the server binary.
For a deep dive into how the system works, see [ARCHITECTURE.md](ARCHITECTURE.md).

Copy `.env.example` to `.env` and configure the required values.

### Core Settings

| Variable | Description | Default |
|----------|-------------|---------|
| `DOMAIN_NAME` | Root domain for your server (e.g. `tunnel.example.com`). Enables HTTPS if set. | *empty* (HTTP) |
| `PROJECT_NAME` | Project name for branding on landing page. | `Go Public` |
| `EMAIL` | Email for Let's Encrypt registration (required if `DOMAIN_NAME` is set). | *empty* |
| `INSECURE_HTTP` | Set to `true` to use HTTP instead of HTTPS (for local dev). | `false` |
| `DB_PATH` | Path to SQLite database file. | `gopublic.db` |
| `CONTROL_PLANE_PORT` | Port for tunnel control plane connections. | `:4443` |

### User Limits

| Variable | Description | Default |
|----------|-------------|---------|
| `DOMAINS_PER_USER` | Number of random domains assigned to each new user. | `2` |
| `DAILY_BANDWIDTH_LIMIT_MB` | Daily bandwidth limit per user in MB (0 = unlimited). | `100` |

### Authentication

| Variable | Description | Default |
|----------|-------------|---------|
| `TELEGRAM_BOT_TOKEN` | Token from @BotFather for Telegram Login. | *empty* |
| `TELEGRAM_BOT_NAME` | Username of your Telegram bot (without @). | *empty* |
| `YANDEX_CLIENT_ID` | Yandex OAuth client ID (register at oauth.yandex.com). | *empty* |
| `YANDEX_CLIENT_SECRET` | Yandex OAuth client secret. | *empty* |

### Notifications & Security

| Variable | Description | Default |
|----------|-------------|---------|
| `ADMIN_TELEGRAM_ID` | Telegram user ID for receiving abuse reports. | *empty* |
| `SESSION_HASH_KEY` | 32-byte hex key for cookie signing. | *random in dev* |
| `SESSION_BLOCK_KEY` | 32-byte hex key for cookie encryption. | *random in dev* |

### Optional

| Variable | Description | Default |
|----------|-------------|---------|
| `GITHUB_REPO` | GitHub repository for client downloads (e.g. `username/gopublic`). | *empty* |

**Example `.env` file:**
```ini
DOMAIN_NAME=tunnel.mysite.com
PROJECT_NAME=My Tunnel
EMAIL=admin@mysite.com
TELEGRAM_BOT_TOKEN=123456:ABC-DEF...
TELEGRAM_BOT_NAME=MyTunnelBot
DOMAINS_PER_USER=3
DAILY_BANDWIDTH_LIMIT_MB=500
```

## VPS Deployment

### 1. Prerequisites

Before deploying to a VPS, ensure you have:
- **Wildcard DNS**: Create a wildcard `A` record (e.g., `*.yourdomain.com`) and a root `A` record (`yourdomain.com`) pointing to your VPS IP.
- **Open Ports**: Ensure your firewall (ufw, iptables, Cloud security groups) allows incoming traffic on:
  - `80/tcp` (HTTP & ACME challenges)
  - `443/tcp` (HTTPS Ingress)
  - `4443/tcp` (Control Plane - Tunnel Connection)
- **Telegram Bot**: Create a bot via [@BotFather](https://t.me/BotFather) and enable "Domain" for the login widget to match your `DOMAIN_NAME`.

## Getting Started

### 1. Server Setup (Docker)

1.  **Create `.env` file**:
    ```ini
    DOMAIN_NAME=tunnel.yourdomain.com
    EMAIL=admin@yourdomain.com
    TELEGRAM_BOT_TOKEN=123456:ABC-DEF...
    TELEGRAM_BOT_NAME=YourBotName
    ```

```bash
docker-compose up -d --build
```

> [!TIP]
> Use `docker-compose logs -f` to check if Let's Encrypt certificates are being successfully issued.

3.  **Access Dashboard**:
    -   Open `https://app.tunnel.yourdomain.com`.
    -   Log in with Telegram.
    -   Copy your **Auth Token**.

### 2. Client Setup

1.  **Build Client**:
    You need to build the client binary pointing to your server address.
    ```bash
    make build-client SERVER_ADDR=tunnel.yourdomain.com:4443
    ```
    *(For local dev use `SERVER_ADDR=localhost:4443`)*

2.  **Authenticate**:
    ```bash
    ./bin/gopublic-client auth <YOUR_TOKEN>
    ```
    This saves the token to `~/.gopublic`.

3.  **Start Tunnel**:
    Expose a local port (e.g., 3000) to the internet:
    ```bash
    ./bin/gopublic-client start 3000
    ```
    
    You will see your public URL (e.g., `https://misty-river.tunnel.yourdomain.com`).

4.  **Inspector**:
    Open `http://localhost:4040` to view the local inspector UI.

---

## Local Development (No Docker)

If you want to run the server locally without Docker/HTTPS:

1.  **Run Server**:
    ```bash
    # Leave DOMAIN_NAME empty in .env or environment
    go run cmd/server/main.go
    ```
    *Server listens on :8080 (HTTP Ingress) and :4443 (TCP Control).*

2.  **Run Client**:
    ```bash
    make build-client SERVER_ADDR=localhost:4443
    ./bin/gopublic-client auth sk_live_12345  # Default seed token
    ./bin/gopublic-client start 8000
    ```

3.  **Test**:
    ```bash
    curl -H "Host: misty-river" http://localhost:8080/
    ```

### Testing Dashboard Locally

To test the **Dashboard** and **Auth** locally:

1.  **Configure `.env`**:
    ```ini
    DOMAIN_NAME=localhost
    INSECURE_HTTP=true
    TELEGRAM_BOT_TOKEN=...
    TELEGRAM_BOT_NAME=...
    ```

2.  **Run Server**:
    ```bash
    go run cmd/server/main.go
    ```

3.  **Access**:
    Open `http://app.localhost:8080` in your browser.
    *(Note: Chrome/Firefox usually resolve `*.localhost` to `127.0.0.1` automatically. If not, add `127.0.0.1 app.localhost` to your `/etc/hosts`.)*

