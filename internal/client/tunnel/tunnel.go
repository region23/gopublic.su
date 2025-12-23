package tunnel

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"gopublic/internal/client/inspector"
	"gopublic/pkg/protocol"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

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
	// For local development, skip TLS if server is localhost/127.0.0.1
	host, _, _ := net.SplitHostPort(t.ServerAddr)
	if host == "" {
		host = t.ServerAddr
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"

	if isLocal {
		log.Printf("Local server detected on %s, using plain TCP", t.ServerAddr)
		conn, err := net.Dial("tcp", t.ServerAddr)
		if err != nil {
			return fmt.Errorf("failed to connect to local server: %v", err)
		}
		return t.handleSession(conn)
	}

	conn, err := tls.Dial("tcp", t.ServerAddr, &tls.Config{
		InsecureSkipVerify: true,
	})

	if err != nil {
		log.Printf("TLS connection failed, trying plain TCP: %v", err)
		connPlain, errPlain := net.Dial("tcp", t.ServerAddr)
		if errPlain != nil {
			return fmt.Errorf("failed to connect: %v", errPlain)
		}
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
		scheme := "https"
		if strings.Contains(t.ServerAddr, "localhost") || strings.Contains(t.ServerAddr, "127.0.0.1") {
			scheme = "http"
		}
		// If server addr has a port (like :80), we might need it in the output too for local dev
		// But usually Ingress is on :80 or :443.
		// If it's local dev, ingress is on :80.
		fmt.Printf(" - %s://%s -> localhost:%s\n", scheme, d, t.LocalPort)
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

	// To support Inspector, we parse the HTTP request
	reader := bufio.NewReader(remote)
	req, err := http.ReadRequest(reader)
	if err != nil {
		// Not a valid HTTP request or error? Just copy TCP.
		go io.Copy(local, remote)
		io.Copy(remote, local)
		return
	}

	// Record request to inspector
	inspector.AddRequest(req.Method, req.Host, req.URL.Path, 0)

	// Forward Request to Local
	if err := req.Write(local); err != nil {
		log.Printf("Failed to write request to local: %v", err)
		return
	}

	// Read Response from Local
	respReader := bufio.NewReader(local)
	resp, err := http.ReadResponse(respReader, req)
	if err != nil {
		log.Printf("Failed to read response from local: %v", err)
		return
	}
	defer resp.Body.Close()

	// Update inspector with status code
	// (Simplistic: we update the last one or by ID? Let's just update the list for now if we had IDs)
	// For MVP, we'll just log the request at the start.

	// Forward Response back to Remote
	if err := resp.Write(remote); err != nil {
		log.Printf("Failed to write response to remote: %v", err)
		return
	}
}
