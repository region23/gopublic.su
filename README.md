# GoPublic

GoPublic is a self-hosted reverse proxy service (similar to ngrok) that allows you to expose local services to the public internet via a secure tunnel.

## Configuration

You can configure the server using **Environment Variables** or a **`.env`** file placed in the same directory as the server binary.
For a deep dive into how the system works, see [ARCHITECTURE.md](ARCHITECTURE.md).

| Variable | Description | Default |
|----------|-------------|---------|
| `DOMAIN_NAME` | The root domain for your server (e.g. `example.com`). If set, enables **HTTPS** mode. | *empty* (HTTP mode) |
| `EMAIL` | Email address for Let's Encrypt registration (required if `DOMAIN_NAME` is set). | *empty* |
| `TELEGRAM_BOT_TOKEN` | Token from @BotFather for Telegram Login. | *empty* |
| `TELEGRAM_BOT_NAME` | Username of your bot (e.g. `MyGopublicBot`) used in the login widget. | *empty* |

**Example `.env` file:**
```ini
DOMAIN_NAME=tunnel.mysite.com
EMAIL=admin@mysite.com
```

## Getting Started

### 1. Server Setup (Docker)

1.  **Create `.env` file**:
    ```ini
    DOMAIN_NAME=tunnel.yourdomain.com
    EMAIL=admin@yourdomain.com
    TELEGRAM_BOT_TOKEN=123456:ABC-DEF...
    TELEGRAM_BOT_NAME=YourBotName
    ```

2.  **Run Server**:
    ```bash
    docker-compose up -d --build
    ```

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

