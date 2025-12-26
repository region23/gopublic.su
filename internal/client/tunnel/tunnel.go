package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gopublic/internal/client/events"
	"gopublic/internal/client/inspector"
	"gopublic/internal/client/logger"
	"gopublic/internal/client/stats"
	"gopublic/pkg/protocol"

	"github.com/hashicorp/yamux"
)

// TLSConfig holds TLS configuration options.
type TLSConfig struct {
	InsecureSkipVerify bool
	ServerName         string
}

// Tunnel represents a connection to the gopublic server.
type Tunnel struct {
	ServerAddr string
	Token      string
	LocalPort  string
	Subdomain  string // Specific subdomain to bind (empty = bind all)
	Force      bool   // Force disconnect existing session

	// TLS configuration
	TLSConfig *TLSConfig

	// Dependencies (optional, for integration with TUI)
	eventBus *events.Bus
	stats    *stats.Stats

	// Internal state for graceful shutdown
	mu          sync.Mutex
	wg          sync.WaitGroup
	activeConns map[net.Conn]struct{}
	session     *yamux.Session
	closed      bool

	// Cached connection info
	boundDomains []string
}

// NewTunnel creates a new tunnel instance.
func NewTunnel(serverAddr, token, localPort string) *Tunnel {
	return &Tunnel{
		ServerAddr:  serverAddr,
		Token:       token,
		LocalPort:   localPort,
		activeConns: make(map[net.Conn]struct{}),
	}
}

// SetEventBus sets the event bus for publishing tunnel events.
func (t *Tunnel) SetEventBus(bus *events.Bus) {
	t.eventBus = bus
}

// SetStats sets the stats tracker for recording metrics.
func (t *Tunnel) SetStats(s *stats.Stats) {
	t.stats = s
}

// SetTLSConfig sets the TLS configuration.
func (t *Tunnel) SetTLSConfig(cfg *TLSConfig) {
	t.TLSConfig = cfg
}

// SetForce sets the force flag to disconnect existing session.
func (t *Tunnel) SetForce(force bool) {
	t.Force = force
}

// BoundDomains returns the domains bound to this tunnel.
func (t *Tunnel) BoundDomains() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.boundDomains
}

// publishEvent safely publishes an event if eventBus is set.
func (t *Tunnel) publishEvent(eventType events.EventType, data interface{}) {
	if t.eventBus != nil {
		t.eventBus.Publish(events.Event{Type: eventType, Data: data})
	}
}

// publishStatus publishes a connection status event.
func (t *Tunnel) publishStatus(stage, message string) {
	t.publishEvent(events.EventConnectionStatus, events.ConnectionStatusData{
		Stage:   stage,
		Message: message,
	})
}

// trackConn adds a connection to the active set.
func (t *Tunnel) trackConn(conn net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activeConns == nil {
		t.activeConns = make(map[net.Conn]struct{})
	}
	t.activeConns[conn] = struct{}{}
}

// untrackConn removes a connection from the active set.
func (t *Tunnel) untrackConn(conn net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.activeConns, conn)
}

// Start establishes a connection to the server and starts the tunnel.
func (t *Tunnel) Start() error {
	t.publishEvent(events.EventConnecting, nil)

	// For local development, skip TLS if server is localhost/127.0.0.1
	host, _, _ := net.SplitHostPort(t.ServerAddr)
	if host == "" {
		host = t.ServerAddr
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"

	connectStart := time.Now()

	// Dial timeout for initial connection
	dialTimeout := 10 * time.Second

	if isLocal {
		t.publishStatus("dialing", fmt.Sprintf("Connecting to %s (plain TCP)...", t.ServerAddr))
		logger.Info("Local server detected on %s, using plain TCP", t.ServerAddr)
		conn, err := net.DialTimeout("tcp", t.ServerAddr, dialTimeout)
		if err != nil {
			t.publishStatus("error", fmt.Sprintf("Connection failed: %v", err))
			t.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "connect"})
			return fmt.Errorf("failed to connect to local server: %v", err)
		}
		return t.handleSession(conn, connectStart)
	}

	// Build TLS config
	tlsConfig := &tls.Config{}
	if t.TLSConfig != nil {
		tlsConfig.InsecureSkipVerify = t.TLSConfig.InsecureSkipVerify
		if t.TLSConfig.ServerName != "" {
			tlsConfig.ServerName = t.TLSConfig.ServerName
		}
	} else {
		// Default: insecure for backward compatibility (TODO: make secure by default)
		tlsConfig.InsecureSkipVerify = true
	}

	t.publishStatus("dialing", fmt.Sprintf("Connecting to %s (TLS)...", t.ServerAddr))
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", t.ServerAddr, tlsConfig)
	if err != nil {
		t.publishStatus("tls_fallback", fmt.Sprintf("TLS failed: %v, trying plain TCP...", err))
		logger.Warn("TLS connection failed, trying plain TCP: %v", err)
		connPlain, errPlain := net.DialTimeout("tcp", t.ServerAddr, dialTimeout)
		if errPlain != nil {
			t.publishStatus("error", fmt.Sprintf("Connection failed: %v", errPlain))
			t.publishEvent(events.EventError, events.ErrorData{Error: errPlain, Context: "connect"})
			return fmt.Errorf("failed to connect: %v", errPlain)
		}
		return t.handleSession(connPlain, connectStart)
	}

	return t.handleSession(conn, connectStart)
}

func (t *Tunnel) handleSession(conn net.Conn, connectStart time.Time) error {
	defer conn.Close()

	// Check if already closed
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errors.New("tunnel is closed")
	}
	t.mu.Unlock()

	// Start Yamux Client
	t.publishStatus("yamux_init", "Initializing multiplexed connection...")
	session, err := yamux.Client(conn, nil)
	if err != nil {
		t.publishStatus("error", fmt.Sprintf("Failed to init yamux: %v", err))
		return fmt.Errorf("failed to start yamux: %v", err)
	}

	// Store session for graceful shutdown
	t.mu.Lock()
	t.session = session
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.session = nil
		t.mu.Unlock()
		session.Close()
	}()

	// Handshake: Open stream for control
	t.publishStatus("handshake", "Opening control stream...")
	stream, err := session.Open()
	if err != nil {
		t.publishStatus("error", fmt.Sprintf("Failed to open stream: %v", err))
		return fmt.Errorf("failed to open handshake stream: %v", err)
	}

	// Set write deadline for handshake operations
	handshakeTimeout := 5 * time.Second
	stream.SetWriteDeadline(time.Now().Add(handshakeTimeout))

	// Auth
	t.publishStatus("authenticating", "Authenticating with server...")
	authReq := protocol.AuthRequest{Token: t.Token, Force: t.Force}
	if err := json.NewEncoder(stream).Encode(authReq); err != nil {
		t.publishStatus("error", fmt.Sprintf("Failed to send auth: %v", err))
		return err
	}

	// Build domain request: specific subdomain or empty (= bind all)
	t.publishStatus("requesting_tunnel", "Requesting tunnel...")
	var requestedDomains []string
	if t.Subdomain != "" {
		requestedDomains = []string{t.Subdomain}
	}
	tunnelReq := protocol.TunnelRequest{RequestedDomains: requestedDomains}
	if err := json.NewEncoder(stream).Encode(tunnelReq); err != nil {
		t.publishStatus("error", fmt.Sprintf("Failed to request tunnel: %v", err))
		return err
	}
	// Clear write deadline
	stream.SetWriteDeadline(time.Time{})

	// Read Response with timeout to prevent hanging
	t.publishStatus("waiting_response", "Waiting for server response...")
	stream.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var resp protocol.InitResponse
	if err := json.NewDecoder(stream).Decode(&resp); err != nil {
		t.publishStatus("error", fmt.Sprintf("Failed to read response: %v", err))
		return fmt.Errorf("handshake read failed: %v", err)
	}
	// Clear deadline for normal operation
	stream.SetReadDeadline(time.Time{})

	if !resp.Success {
		// Check for specific error code
		if resp.ErrorCode == protocol.ErrorCodeAlreadyConnected {
			t.publishStatus("error", fmt.Sprintf("Already connected: %s", resp.Error))
			return &AlreadyConnectedError{Message: resp.Error}
		}
		t.publishStatus("error", fmt.Sprintf("Server error: %s", resp.Error))
		return fmt.Errorf("server error: %s", resp.Error)
	}

	// Calculate latency and record stats
	latency := time.Since(connectStart)
	if t.stats != nil {
		t.stats.SetServerLatency(latency)
	}

	// Cache bound domains
	t.mu.Lock()
	t.boundDomains = resp.BoundDomains
	t.mu.Unlock()

	// Determine scheme for display
	scheme := "https"
	if strings.Contains(t.ServerAddr, "localhost") || strings.Contains(t.ServerAddr, "127.0.0.1") {
		scheme = "http"
	}

	// Publish connected event with server stats
	connData := events.ConnectedData{
		ServerAddr:   t.ServerAddr,
		BoundDomains: resp.BoundDomains,
		Latency:      latency,
	}
	if resp.ServerStats != nil {
		connData.BandwidthToday = resp.ServerStats.BandwidthToday
		connData.BandwidthTotal = resp.ServerStats.BandwidthTotal
		connData.BandwidthLimit = resp.ServerStats.BandwidthLimit
	}
	t.publishEvent(events.EventConnected, connData)

	// Publish tunnel ready event for each domain
	for _, d := range resp.BoundDomains {
		t.publishEvent(events.EventTunnelReady, events.TunnelReadyData{
			LocalPort:    t.LocalPort,
			BoundDomains: []string{d},
			Scheme:       scheme,
		})
	}

	// Log to console (legacy output, will be replaced by TUI)
	fmt.Printf("\nTunnel Established! Incoming traffic on:\n")
	for _, d := range resp.BoundDomains {
		fmt.Printf(" - %s://%s -> localhost:%s\n", scheme, d, t.LocalPort)
	}
	stream.Close() // Handshake done

	// Accept Streams with proper tracking
	for {
		stream, err := session.Accept()
		if err != nil {
			// Check if this is a graceful shutdown
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if closed {
				// Wait for all proxy goroutines to finish
				t.wg.Wait()
				return nil
			}
			t.publishEvent(events.EventDisconnected, nil)
			return fmt.Errorf("session ended: %v", err)
		}

		// Track goroutine to prevent leaks
		t.wg.Add(1)
		go func(s net.Conn) {
			defer t.wg.Done()
			t.proxyStream(s)
		}(stream)
	}
}

func (t *Tunnel) proxyStream(remote net.Conn) {
	defer remote.Close()
	startTime := time.Now()

	// Track connection for stats
	if t.stats != nil {
		t.stats.IncrementConnections()
		defer t.stats.DecrementOpenConnections()
	}

	// Track active connection for graceful shutdown
	t.trackConn(remote)
	defer t.untrackConn(remote)

	// Dial Local
	local, err := net.Dial("tcp", "localhost:"+t.LocalPort)
	if err != nil {
		logger.Error("Failed to dial local port %s: %v", t.LocalPort, err)
		t.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "dial_local"})
		return
	}
	defer local.Close()

	// To support Inspector, we parse the HTTP request
	reader := bufio.NewReader(remote)
	req, err := http.ReadRequest(reader)
	if err != nil {
		// Not a valid HTTP request or error? Just copy TCP bidirectionally
		t.copyBidirectional(local, remote)
		return
	}

	// Publish request start event
	t.publishEvent(events.EventRequestStart, events.RequestData{
		Method: req.Method,
		Path:   req.URL.Path,
	})

	// Buffer request body for inspector (with error handling)
	var reqBody []byte
	if req.Body != nil {
		var readErr error
		reqBody, readErr = io.ReadAll(req.Body)
		if readErr != nil {
			logger.Warn("Failed to read request body: %v", readErr)
			// Continue with empty body rather than silent failure
			reqBody = []byte{}
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	// Forward Request to Local
	if err := req.Write(local); err != nil {
		logger.Error("Failed to write request to local: %v", err)
		t.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "write_request"})
		return
	}

	// Read Response from Local
	respReader := bufio.NewReader(local)
	resp, err := http.ReadResponse(respReader, req)
	if err != nil {
		logger.Error("Failed to read response from local: %v", err)
		// Record failed request to inspector
		inspector.AddExchange(req, reqBody, nil, nil, time.Since(startTime))
		t.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "read_response"})
		return
	}
	defer resp.Body.Close()

	// Buffer response body for inspector (with error handling)
	var respBody []byte
	if resp.Body != nil {
		var readErr error
		respBody, readErr = io.ReadAll(resp.Body)
		if readErr != nil {
			logger.Warn("Failed to read response body: %v", readErr)
			// Continue with empty body rather than silent failure
			respBody = []byte{}
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}

	duration := time.Since(startTime)
	totalBytes := int64(len(reqBody) + len(respBody))

	// Record complete exchange to inspector
	inspector.AddExchange(req, reqBody, resp, respBody, duration)

	// Record stats
	if t.stats != nil {
		t.stats.RecordRequest(duration, totalBytes)
	}

	// Publish request complete event
	t.publishEvent(events.EventRequestComplete, events.RequestData{
		Method:   req.Method,
		Path:     req.URL.Path,
		Status:   resp.StatusCode,
		Duration: duration,
		Bytes:    totalBytes,
	})

	// Forward Response back to Remote
	if err := resp.Write(remote); err != nil {
		logger.Error("Failed to write response to remote: %v", err)
		t.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "write_response"})
		return
	}
}

// copyBidirectional copies data between two connections with proper error handling.
// This is used for non-HTTP traffic.
func (t *Tunnel) copyBidirectional(local, remote net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Remote -> Local
	go func() {
		defer wg.Done()
		_, err := io.Copy(local, remote)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			logger.Warn("Error copying remote->local: %v", err)
		}
		// Half-close: signal EOF to local
		if tcpConn, ok := local.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	// Local -> Remote
	go func() {
		defer wg.Done()
		_, err := io.Copy(remote, local)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			logger.Warn("Error copying local->remote: %v", err)
		}
		// Half-close: signal EOF to remote
		if tcpConn, ok := remote.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
}

// Shutdown gracefully shuts down the tunnel, waiting for active connections.
func (t *Tunnel) Shutdown(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true

	// Close the session to stop accepting new streams
	if t.session != nil {
		t.session.Close()
	}

	// Close all active connections
	for conn := range t.activeConns {
		conn.Close()
	}
	t.mu.Unlock()

	// Wait for all goroutines with timeout
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
