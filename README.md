# GoPublic

GoPublic is a self-hosted reverse proxy service (similar to ngrok) that allows you to expose local services to the public internet via a secure tunnel.

## Configuration

You can configure the server using **Environment Variables** or a **`.env`** file placed in the same directory as the server binary.
For a deep dive into how the system works, see [ARCHITECTURE.md](ARCHITECTURE.md).

| Variable | Description | Default |
|----------|-------------|---------|
| `DOMAIN_NAME` | The root domain for your server (e.g. `example.com`). If set, enables **HTTPS** mode. | *empty* (HTTP mode) |
| `EMAIL` | Email address for Let's Encrypt registration (required if `DOMAIN_NAME` is set). | *empty* |

**Example `.env` file:**
```ini
DOMAIN_NAME=tunnel.mysite.com
EMAIL=admin@mysite.com
```

## Local Development & Testing

You can test the server locally using the included mock client.

### Prerequisites
- Go 1.22+

### 1. Start the Server
The server will default to listening on port `:4443` for control connections and `:8080` for HTTP ingress (to avoid permission issues on local machines).

```bash
go run cmd/server/main.go
```
*Note: The first time you run this, it will create a `gopublic.db` SQLite database and seed it with a test user.*

### 2. Run the Mock Client
Open a new terminal and run the mock client. It connects to the local server, authenticates with the test token, and tunnels requests to a simulated local service.

```bash
go run cmd/mock_client/main.go
```

### 3. Test the Connection
Use `curl` to send a request to the server's ingress port, specifying the hostname that the mock client registered (`misty-river`).

```bash
curl -v -H "Host: misty-river" http://localhost:8080/path
```

You should see: `Hello from Mock Client!`

---

## Deployment on VPS (Docker)

The recommended way to deploy the server is using Docker.

### 1. Prerequisites
- Docker and Docker Compose installed on your VPS.

### 2. Deployment Steps

1. **Clone/Copy Project**: Copy the project files to your VPS.
2. **Configure Domain**:
   Open `docker-compose.yml` and add your domain and email under `environment`:

```yaml
    environment:
      - DOMAIN_NAME=example.com # Your root domain
      - EMAIL=your-email@example.com # For Let's Encrypt
```

3. **Build and Run**:

```bash
docker-compose up -d --build
```

This will:
- Build the Go binary in a container.
- Start the server container.
- Bind port **4443** (Control Plane).
- Bind host port **80** (HTTP Redirect) and **443** (HTTPS).
- Persist data (users, active tunnels) and **SSL Certificates** in the `./data` directory.


### 3. Verify
Check logs to ensure it started correctly:
```bash
docker-compose logs -f
```

You should see:
```
Control Plane listening on :4443
Public Ingress listening on :8080
```

---

## Deployment on VPS (Manual Binary)

If you prefer running the binary directly:

1. **Build**: `GOOS=linux GOARCH=amd64 go build -o server cmd/server/main.go`
2. **Upload**: `scp server user@host:~`
3. **Run**: `./server` (Use `systemd` for persistence).

