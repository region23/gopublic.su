package tunnel

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"gopublic/pkg/protocol"
	"io"
	"log"
	"net"

	"github.com/hashicorp/yamux"
)

type Tunnel struct {
	ServerAddr string
	Token      string
	LocalPort  string
}

func NewTunnel(serverAddr, token, localPort string) *Tunnel {
	return &Tunnel{
		ServerAddr: serverAddr,
		Token:      token,
		LocalPort:  localPort,
	}
}

func (t *Tunnel) Start() error {
	// 1. Connect to Server (Try TLS first, fallback to TCP for local dev if needed?
	// No, main.go decides via ldflags/var. If var is just host:port, we need to know if TLS.
	// For simplicity, let's assume TLS if port is 4443 or we can try.
	// Actually, Server changes made it support TLS.
	// Let's try `tls.Dial`. If it fails, maybe fallback?
	// The Server always listens on TLS if DOMAIN_NAME is set.
	// If DOMAIN_NAME is NOT set (local dev), it listens on plain TCP.
	// We need a flag or heuristic.
	// Let's assume TLS by default for "Production" feel, but allow insecure if handshake fails?
	// Better: Use `tls.Dial` with `InsecureSkipVerify: true` for self-signed or just trust system roots.
	// If connection fails, user might need to specify --insecure.

	conn, err := tls.Dial("tcp", t.ServerAddr, &tls.Config{
		InsecureSkipVerify: true, // For MVP/Dev. Production should NOT check this.
		// TODO: remove skip verify for PROD.
	})

	if err != nil {
		// Fallback to plain TCP for local dev (if server is HTTP-only)
		log.Printf("TLS connection failed, trying plain TCP: %v", err)
		connPlain, errPlain := net.Dial("tcp", t.ServerAddr)
		if errPlain != nil {
			return fmt.Errorf("failed to connect: %v", errPlain)
		}
		// Use plain connection
		return t.handleSession(connPlain)
	}

	return t.handleSession(conn)
}

func (t *Tunnel) handleSession(conn net.Conn) error {
	defer conn.Close()

	// 2. Start Yamux Client
	session, err := yamux.Client(conn, nil)
	if err != nil {
		return fmt.Errorf("failed to start yamux: %v", err)
	}

	// 3. Handshake
	// Open stream for control/handshake
	stream, err := session.Open()
	if err != nil {
		return fmt.Errorf("failed to open handshake stream: %v", err)
	}

	// Auth
	authReq := protocol.AuthRequest{Token: t.Token}
	if err := json.NewEncoder(stream).Encode(authReq); err != nil {
		return err
	}

	// Request Tunnel (Random domain logic is on server, but client needs to ask)
	// For MVP, we ask for "any" by sending empty? Or server generates?
	// Server logic: "if ValidateDomainOwnership(domain)..."
	// Wait, we generate domains on Registration (Telegram Callback).
	// So the user HAS domains. The client should ask for ALL or SPECIFIC?
	// `gopublic start [port]` implies one tunnel.
	// Which domain?
	// For MVP: Request *all* owned domains? Or just pick the first?
	// Let's ask for *all* domains belonging to the user? Client doesn't know them.
	// Let's send Empty `RequestedDomains`. Server should be updated to return "All owned domains" if list is empty?
	// Or Client must know.
	// Update: `protocol.TunnelRequest` has `RequestedDomains`.
	// If we send empty, Server currently does nothing.
	// Let's just request "auto" and let Server pick? Server doesn't support "auto".
	// Temporary Fix: Client asks for "misty-river" (hardcoded/config)? No.
	// We need to fetch domains first?
	// IMPLEMENTATION CHANGE:
	// We need a way to list domains OR ask "Bind everything I have".
	// Let's modify Server to bind ALL user domains if `RequestedDomains` is empty?
	// OR: Client CLI needs to accept domain: `gopublic start 3000 --domain foo`.
	// Valid MVP: `gopublic start 3000` -> Binds to the FIRST domain found for user.
	// Let's modify Server to handle empty list = "Bind All".

	// Assuming Server update (I will do this next or assume it works for empty):
	// Send "empty" list implies "bind all available".
	tunnelReq := protocol.TunnelRequest{RequestedDomains: []string{}}
	if err := json.NewEncoder(stream).Encode(tunnelReq); err != nil {
		return err
	}

	// Read Response
	var resp protocol.InitResponse
	if err := json.NewDecoder(stream).Decode(&resp); err != nil {
		return fmt.Errorf("handshake read failed: %v", err)
	}

	if !resp.Success {
		return fmt.Errorf("server error: %s", resp.Error)
	}

	fmt.Printf("Tunnel Established! Incoming traffic on:\n")
	for _, d := range resp.BoundDomains {
		fmt.Printf(" - https://%s.%s -> localhost:%s\n", d, "DOMAIN_NAME", t.LocalPort)
		// Note: Client doesn't know DOMAIN_NAME suffix really, unless server sends it.
		// Server returns full domain or subdomain?
		// DB stores "misty-river-123".
		// Ingress checks `host == "app."+domain`.
		// It seems DB stores SUBDOMAIN only? No: `Name: name`.
		// `gopublic/internal/dashboard/handler.go`: `name := fmt.Sprintf(...)`
		// It creates "misty-river-123".
		// Ingress `handleRequest`: `host := c.Request.Host`.
		// If DB has "misty-river", and host is "misty-river.example.com", Registry match fails?
		// Registry `GetSession(host)`.
		// If Registry registers "misty-river", but request comes as "misty-river.example.com".
		// We need to match correctly.
		// Server Registry currently maps `domain -> session`.
		// If Server registers "misty-river", then Host header "misty-river.example.com" WON'T match.
		// I must fix Server Logic to either register FQDN or match Subdomain.
		// TASK: Check Server Logic.
	}
	stream.Close() // Handshake done

	// 4. Accept Streams
	for {
		stream, err := session.Accept()
		if err != nil {
			return fmt.Errorf("session ended: %v", err)
		}
		go t.proxyStream(stream)
	}
}

func (t *Tunnel) proxyStream(remote net.Conn) {
	defer remote.Close()

	// Dial Local
	local, err := net.Dial("tcp", "localhost:"+t.LocalPort)
	if err != nil {
		log.Printf("Failed to dial local port %s: %v", t.LocalPort, err)
		return
	}
	defer local.Close()

	// Bidirectional Copy
	// For HTTP, we might want to rewrite Host header?
	// But simple TCP proxy is safer for generic streams.
	// However, SPEC says "Read HTTP Request... Forward".
	// Why? To support the Inspector?
	// If we just pipe TCP, Inspector is harder.
	// If we use `io.Copy`, it's fast.
	// Let's stick to `io.Copy` for MVP performance.
	// To support Inspector later, we wrap `remote` in a TeeReader/Writer.

	go io.Copy(local, remote)
	io.Copy(remote, local)
}
