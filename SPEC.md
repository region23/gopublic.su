# GoPublic System Specification

## 1. Overview
GoPublic is a self-hosted reverse proxy service similar to ngrok. It allows exposing local services to the public internet via a secure tunnel.
The system consists of three main components:
- **Server**: Hosted on a public VPS. Handles public HTTP/HTTPS traffic, user authentication, and tunnel management.
- **Client (Agent)**: Runs on the user's local machine. Establishes a persistent connection to the Server and proxies traffic.
- **Web Dashboard**: A web interface for users to register, view tokens, and manage domains.

## 2. User Management & Authentication

### 2.1 Registration Flow
1. **SSO Only**: Users register/login via **Google OAuth**.
2. **Account Creation**: Upon first login:
    - A unique **User ID** is generated.
    - A unique **Auth Token** is generated (e.g., `sk_live_xYz123...`). This token effectively *is* the user identity for the CLI.
    - **Domain Assignment**: The system automatically generates **3 random, memorable subdomains** (e.g., `misty-river`, `silent-star`, `bold-eagle`) and assigns them to the user.
3. **Dashboard display**: The user is redirected to the dashboard where they see:
    - Their Auth Token (with a copy button).
    - Their assigned domains.
    - Setup instructions.

### 2.2 Token Management
- **Token Generation**: Secure random string, generated once at registration.
- **Persistance**:
    - **Server-side**: Stored in database linked to User ID.
    - **Client-side**: User runs `gopublic auth <token>`. The client saves this token to a persistent config file (e.g., `~/.config/gopublic/auth.yml` or `~/.gopublic_token`).
- **Authorization**: Every tunnel connection handshake includes this token. The server validates ownership of requested subdomains against this token.

## 3. Architecture & Protocol

### 3.1 Transport Layer
- **Control Plane**: TCP connection on port `:4443`.
- **Multiplexing**: Uses `yamux` over the single TCP connection.
- **Security**: TLS for Control Plane is required.

### 3.2 Connection Lifecycle
1. **Connect**: Client initiates TCP connection to Server.
2. **Handshake (Stream 1)**:
    - Client sends `AuthRequest` (Token).
    - Server verifies token.
    - Client sends `TunnelRequest` (List of Requested Domains + Local Ports).
    - Server verifies user owns these domains.
    - Server responds with `InitResponse`.
3. **Data Transfer**:
    - Incoming public request -> Server -> Selects Session -> New Yamux Stream -> Client.
    - Client reads Stream -> Proxies to Localhost Port based on mapping.

## 4. Server Specification

### 4.1 Responsibilities
- **Frontend**: Serve Web Dashboard (React/HTML) and handle OAuth callback.
- **Public Ingress**: Listen on `:80` (HTTP) and `:443` (HTTPS).
- **Certificate Management**: Automatic Let's Encrypt certificates (Wildcard `*.gopublic.com` preferred, or On-Demand).
- **Tunnel Registry**: In-memory map of `Hostname -> Session`.

### 4.2 Database
Minimal database (SQLite) required for:
- Users (Email, GoogleID, CreatedAt)
- Tokens (UserID, TokenString)
- Domains (UserID, SubdomainName)

## 5. Client Specification

### 5.1 CLI Commands
- `gopublic auth <token>`: Saves token to config file.
- `gopublic start`: Reads `gopublic.yaml` in current dir and starts tunnels.
- `gopublic start --all`: Start all defined tunnels.

### 5.2 Configuration (`gopublic.yaml`)
The client supports "Projects" via YAML files. A user can map their assigned domains to different local services.

```yaml
version: "1"
tunnels:
  # Map 'misty-river' (assigned domain) to local React app
  frontend:
    proto: http
    addr: 3000
    subdomain: misty-river 

  # Map 'silent-star' (assigned domain) to local API
  backend:
    proto: http
    addr: 8080
    subdomain: silent-star
```

### 5.3 Local Inspection UI (The "Inspector")
Duplicate the "Killer Feature" of ngrok.
- **Listen Address**: `localhost:4040` (by default).
- **Web Interface**:
    - **Traffic Log**: Real-time list of all incoming requests (Method, Path, Status, Duration).
    - **Detail View**: Click a request to see full Headers, Body (JSON/Text), and Response.
    - **Replay**: Button to "Replay" a selected request against the local server without resending from the internet.

## 6. Security Considerations
- **Token Secrecy**: Tokens allow anyone to host on user's domains.
- **Domain Verification**: Server MUST enforce that a token can only bind subdomains assigned to that user.


Language: Golang
Соблюдать принципы KISS и DRY при разработке.
Действовать как senior golang backend developer
Для базы данных использовать sqlite
Если для требуемого функционала есть готовая библиотека, то использовать её.

Ключевые библиотеки:
Мультиплексирование: github.com/hashicorp/yamux
Зачем: Ngrok работает через один постоянный TCP-канал между клиентом и сервером. Чтобы пробрасывать через него много одновременных HTTP-запросов, не открывая новые соединения, нужно мультиплексирование.

CLI: github.com/spf13/cobra. Стандарт для создания CLI-интерфейсов.

HTTP/Routing: github.com/gin-gonic/gin для обработки входящих HTTP-запросов на сервере.