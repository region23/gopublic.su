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

// SharedTunnel represents a single connection to the server with multiple tunnels.
// Each tunnel maps a subdomain to a local port.
type SharedTunnel struct {
	ServerAddr string
	Token      string
	Force      bool
	NoCache    bool              // Add Cache-Control: no-store to responses
	Tunnels    map[string]string // subdomain -> localPort

	// TLS configuration
	TLSConfig *TLSConfig

	// Dependencies
	eventBus *events.Bus
	stats    *stats.Stats

	// Internal state
	mu          sync.Mutex
	wg          sync.WaitGroup
	activeConns map[net.Conn]struct{}
	session     *yamux.Session
	closed      bool

	// Cached connection info
	boundDomains []string
}

// NewSharedTunnel creates a new shared tunnel instance.
func NewSharedTunnel(serverAddr, token string, tunnels map[string]string) *SharedTunnel {
	return &SharedTunnel{
		ServerAddr:  serverAddr,
		Token:       token,
		Tunnels:     tunnels,
		activeConns: make(map[net.Conn]struct{}),
	}
}

// SetEventBus sets the event bus for publishing tunnel events.
func (st *SharedTunnel) SetEventBus(bus *events.Bus) {
	st.eventBus = bus
}

// SetStats sets the stats tracker for recording metrics.
func (st *SharedTunnel) SetStats(s *stats.Stats) {
	st.stats = s
}

// SetTLSConfig sets the TLS configuration.
func (st *SharedTunnel) SetTLSConfig(cfg *TLSConfig) {
	st.TLSConfig = cfg
}

// SetForce sets the force flag to disconnect existing session.
func (st *SharedTunnel) SetForce(force bool) {
	st.Force = force
}

// SetNoCache enables Cache-Control: no-store header on all responses.
func (st *SharedTunnel) SetNoCache(noCache bool) {
	st.NoCache = noCache
}

// BoundDomains returns the domains bound to this tunnel.
func (st *SharedTunnel) BoundDomains() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.boundDomains
}

// publishEvent safely publishes an event if eventBus is set.
func (st *SharedTunnel) publishEvent(eventType events.EventType, data interface{}) {
	if st.eventBus != nil {
		st.eventBus.Publish(events.Event{Type: eventType, Data: data})
	}
}

// publishStatus publishes a connection status event.
func (st *SharedTunnel) publishStatus(stage, message string) {
	st.publishEvent(events.EventConnectionStatus, events.ConnectionStatusData{
		Stage:   stage,
		Message: message,
	})
}

// trackConn adds a connection to the active set.
func (st *SharedTunnel) trackConn(conn net.Conn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.activeConns == nil {
		st.activeConns = make(map[net.Conn]struct{})
	}
	st.activeConns[conn] = struct{}{}
}

// untrackConn removes a connection from the active set.
func (st *SharedTunnel) untrackConn(conn net.Conn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.activeConns, conn)
}

// Start establishes a connection to the server and starts the shared tunnel.
func (st *SharedTunnel) Start(ctx context.Context) error {
	st.publishEvent(events.EventConnecting, nil)

	host, _, _ := net.SplitHostPort(st.ServerAddr)
	if host == "" {
		host = st.ServerAddr
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"

	connectStart := time.Now()
	dialTimeout := 10 * time.Second

	if isLocal {
		st.publishStatus("dialing", fmt.Sprintf("Connecting to %s (plain TCP)...", st.ServerAddr))
		logger.Info("Local server detected on %s, using plain TCP", st.ServerAddr)
		conn, err := net.DialTimeout("tcp", st.ServerAddr, dialTimeout)
		if err != nil {
			st.publishStatus("error", fmt.Sprintf("Connection failed: %v", err))
			st.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "connect"})
			return fmt.Errorf("failed to connect to local server: %v", err)
		}
		return st.handleSession(ctx, conn, connectStart)
	}

	// Build TLS config
	tlsConfig := &tls.Config{}
	if st.TLSConfig != nil {
		tlsConfig.InsecureSkipVerify = st.TLSConfig.InsecureSkipVerify
		if st.TLSConfig.ServerName != "" {
			tlsConfig.ServerName = st.TLSConfig.ServerName
		}
	} else {
		tlsConfig.InsecureSkipVerify = true
	}

	st.publishStatus("dialing", fmt.Sprintf("Connecting to %s (TLS)...", st.ServerAddr))
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", st.ServerAddr, tlsConfig)
	if err != nil {
		st.publishStatus("tls_fallback", fmt.Sprintf("TLS failed: %v, trying plain TCP...", err))
		logger.Warn("TLS connection failed, trying plain TCP: %v", err)
		connPlain, errPlain := net.DialTimeout("tcp", st.ServerAddr, dialTimeout)
		if errPlain != nil {
			st.publishStatus("error", fmt.Sprintf("Connection failed: %v", errPlain))
			st.publishEvent(events.EventError, events.ErrorData{Error: errPlain, Context: "connect"})
			return fmt.Errorf("failed to connect: %v", errPlain)
		}
		return st.handleSession(ctx, connPlain, connectStart)
	}

	return st.handleSession(ctx, conn, connectStart)
}

func (st *SharedTunnel) handleSession(ctx context.Context, conn net.Conn, connectStart time.Time) error {
	defer conn.Close()

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return errors.New("tunnel is closed")
	}
	st.mu.Unlock()

	// Start Yamux Client
	st.publishStatus("yamux_init", "Initializing multiplexed connection...")
	session, err := yamux.Client(conn, nil)
	if err != nil {
		st.publishStatus("error", fmt.Sprintf("Failed to init yamux: %v", err))
		return fmt.Errorf("failed to start yamux: %v", err)
	}

	st.mu.Lock()
	st.session = session
	st.mu.Unlock()

	defer func() {
		st.mu.Lock()
		st.session = nil
		st.mu.Unlock()
		session.Close()
	}()

	// Watch for context cancellation to close session
	go func() {
		<-ctx.Done()
		session.Close()
	}()

	// Handshake
	st.publishStatus("handshake", "Opening control stream...")
	stream, err := session.Open()
	if err != nil {
		st.publishStatus("error", fmt.Sprintf("Failed to open stream: %v", err))
		return fmt.Errorf("failed to open handshake stream: %v", err)
	}

	handshakeTimeout := 5 * time.Second
	stream.SetWriteDeadline(time.Now().Add(handshakeTimeout))

	// Auth
	st.publishStatus("authenticating", "Authenticating with server...")
	authReq := protocol.AuthRequest{Token: st.Token, Force: st.Force}
	if err := json.NewEncoder(stream).Encode(authReq); err != nil {
		st.publishStatus("error", fmt.Sprintf("Failed to send auth: %v", err))
		return err
	}

	// Request all subdomains
	st.publishStatus("requesting_tunnel", "Requesting tunnels...")
	var requestedDomains []string
	for subdomain := range st.Tunnels {
		requestedDomains = append(requestedDomains, subdomain)
	}
	tunnelReq := protocol.TunnelRequest{RequestedDomains: requestedDomains}
	if err := json.NewEncoder(stream).Encode(tunnelReq); err != nil {
		st.publishStatus("error", fmt.Sprintf("Failed to request tunnel: %v", err))
		return err
	}
	stream.SetWriteDeadline(time.Time{})

	// Read response
	stream.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var resp protocol.InitResponse
	if err := json.NewDecoder(stream).Decode(&resp); err != nil {
		st.publishStatus("error", fmt.Sprintf("Failed to read response: %v", err))
		return err
	}
	stream.SetReadDeadline(time.Time{})

	if !resp.Success {
		st.publishStatus("error", resp.Error)
		if resp.ErrorCode == protocol.ErrorCodeAlreadyConnected {
			return &AlreadyConnectedError{Message: resp.Error}
		}
		return fmt.Errorf("server error: %s", resp.Error)
	}

	// Store bound domains
	st.mu.Lock()
	st.boundDomains = resp.BoundDomains
	st.mu.Unlock()

	// Calculate latency
	latency := time.Since(connectStart)

	// Publish connected event with server stats
	connectedData := events.ConnectedData{
		BoundDomains: resp.BoundDomains,
		Latency:      latency,
	}
	if resp.ServerStats != nil {
		connectedData.BandwidthToday = resp.ServerStats.BandwidthToday
		connectedData.BandwidthTotal = resp.ServerStats.BandwidthTotal
		connectedData.BandwidthLimit = resp.ServerStats.BandwidthLimit
	}
	st.publishEvent(events.EventConnected, connectedData)

	// Determine scheme (https for remote, http for local)
	host, _, _ := net.SplitHostPort(st.ServerAddr)
	if host == "" {
		host = st.ServerAddr
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"
	scheme := "https"
	if isLocal {
		scheme = "http"
	}

	// Publish TunnelReady for each subdomain -> localPort mapping
	// This populates the Forwarding section in TUI
	for subdomain, localPort := range st.Tunnels {
		// Find matching bound domain for this subdomain
		var boundDomainsForTunnel []string
		for _, bd := range resp.BoundDomains {
			if strings.HasPrefix(bd, subdomain+".") || bd == subdomain {
				boundDomainsForTunnel = append(boundDomainsForTunnel, bd)
			}
		}
		if len(boundDomainsForTunnel) == 0 {
			// Fallback: use any bound domain that starts with subdomain
			for _, bd := range resp.BoundDomains {
				if strings.Contains(bd, subdomain) {
					boundDomainsForTunnel = append(boundDomainsForTunnel, bd)
					break
				}
			}
		}
		if len(boundDomainsForTunnel) > 0 {
			st.publishEvent(events.EventTunnelReady, events.TunnelReadyData{
				Name:         subdomain,
				LocalPort:    localPort,
				BoundDomains: boundDomainsForTunnel,
				Scheme:       scheme,
			})
		}
	}

	// Accept incoming streams
	st.acceptStreams(session)

	return nil
}

// acceptStreams accepts incoming streams from the server and routes them.
func (st *SharedTunnel) acceptStreams(session *yamux.Session) {
	for {
		stream, err := session.Accept()
		if err != nil {
			if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "session shutdown") {
				logger.Error("Session error: %v", err)
				st.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "session"})
			}
			return
		}

		st.wg.Add(1)
		go func(s net.Conn) {
			defer st.wg.Done()
			st.proxyStream(s)
		}(stream)
	}
}

// proxyStream routes a stream to the correct local port based on Host header.
func (st *SharedTunnel) proxyStream(remote net.Conn) {
	defer remote.Close()
	startTime := time.Now()

	if st.stats != nil {
		st.stats.IncrementConnections()
		defer st.stats.DecrementOpenConnections()
	}

	st.trackConn(remote)
	defer st.untrackConn(remote)

	// Read HTTP request to determine the target port
	reader := bufio.NewReader(remote)
	req, err := http.ReadRequest(reader)
	if err != nil {
		// Not HTTP - can't route without Host header
		logger.Warn("Failed to parse HTTP request for routing: %v", err)
		return
	}

	// Extract subdomain from Host header
	localPort := st.getLocalPortForHost(req.Host)
	if localPort == "" {
		logger.Warn("No tunnel configured for host: %s", req.Host)
		// Send 502 Bad Gateway response
		resp := &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("No tunnel configured for this host")),
		}
		resp.Header.Set("Content-Type", "text/plain")
		resp.Write(remote)
		return
	}

	// Dial local port (localPort may be "port" or "host:port")
	localAddr := resolveLocalAddr(localPort)
	local, err := net.Dial("tcp", localAddr)
	if err != nil {
		friendlyMsg := formatLocalDialError(localAddr, err)
		logger.Error("%s", friendlyMsg)
		st.publishEvent(events.EventError, events.ErrorData{Error: fmt.Errorf("%s", friendlyMsg), Context: "dial_local"})
		return
	}
	defer local.Close()

	// Publish request start event
	if req.Header.Get("X-GoPublic-Control") == "bandwidth_exceeded" || strings.HasPrefix(req.URL.Path, "/__gopublic/control/") {
		st.publishEvent(events.EventLog, events.LogData{Level: "warn", Message: "Дневной лимит трафика исчерпан: внешний URL тоннеля будет отдавать 429 до завтра."})
		resp := &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}
		_ = resp.Write(remote)
		return
	}

	st.publishEvent(events.EventRequestStart, events.RequestData{Method: req.Method, Path: req.URL.Path})

	// Buffer request body for inspector
	var reqBody []byte
	if req.Body != nil {
		var readErr error
		reqBody, readErr = io.ReadAll(req.Body)
		if readErr != nil {
			logger.Warn("Failed to read request body: %v", readErr)
			reqBody = []byte{}
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	// Forward request to local
	if err := req.Write(local); err != nil {
		logger.Error("Failed to write request to local: %v", err)
		st.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "write_request"})
		return
	}

	// Read response from local
	respReader := bufio.NewReader(local)
	resp, err := http.ReadResponse(respReader, req)
	if err != nil {
		logger.Error("Failed to read response from local: %v", err)
		inspector.AddExchange(req, reqBody, nil, nil, time.Since(startTime))
		st.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "read_response"})
		return
	}

	// Check if this is a WebSocket upgrade (101 Switching Protocols)
	isUpgrade := strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")

	if isUpgrade && resp.StatusCode == http.StatusSwitchingProtocols {
		// This is a successful WebSocket upgrade
		// Add Cache-Control header if --no-cache flag is set
		if st.NoCache {
			resp.Header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
		}

		// Forward the 101 response to remote
		if err := resp.Write(remote); err != nil {
			logger.Error("Failed to write upgrade response to remote: %v", err)
			st.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "write_response"})
			resp.Body.Close()
			return
		}

		// Record the upgrade in inspector (without body buffering)
		inspector.AddExchange(req, reqBody, resp, []byte("[WebSocket streaming]"), time.Since(startTime))

		// Publish upgrade event
		st.publishEvent(events.EventRequestComplete, events.RequestData{
			Method:   req.Method,
			Path:     req.URL.Path,
			Status:   resp.StatusCode,
			Duration: time.Since(startTime),
			Bytes:    0, // Can't measure WebSocket traffic here
		})

		// Now switch to bidirectional copying
		// Use respReader to preserve any buffered data from local
		st.copyBidirectionalWithReader(remote, local, respReader)
		return
	}

	// Normal HTTP response - buffer and record for inspector
	defer resp.Body.Close()

	// Buffer response body for inspector
	var respBody []byte
	if resp.Body != nil {
		var readErr error
		respBody, readErr = io.ReadAll(resp.Body)
		if readErr != nil {
			logger.Warn("Failed to read response body: %v", readErr)
			respBody = []byte{}
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}

	// Record to inspector
	duration := time.Since(startTime)
	inspector.AddExchange(req, reqBody, resp, respBody, duration)

	// Calculate total bytes
	totalBytes := int64(len(reqBody) + len(respBody))
	for name, values := range req.Header {
		totalBytes += int64(len(name))
		for _, v := range values {
			totalBytes += int64(len(v))
		}
	}
	for name, values := range resp.Header {
		totalBytes += int64(len(name))
		for _, v := range values {
			totalBytes += int64(len(v))
		}
	}

	// Record stats
	if st.stats != nil {
		st.stats.RecordRequest(duration, totalBytes)
	}

	// Publish request complete event
	st.publishEvent(events.EventRequestComplete, events.RequestData{
		Method:   req.Method,
		Path:     req.URL.Path,
		Status:   resp.StatusCode,
		Duration: duration,
		Bytes:    totalBytes,
	})

	// Add Cache-Control header if --no-cache flag is set
	if st.NoCache {
		resp.Header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	}

	// Forward response back to remote
	if err := resp.Write(remote); err != nil {
		logger.Error("Failed to write response to remote: %v", err)
		st.publishEvent(events.EventError, events.ErrorData{Error: err, Context: "write_response"})
		return
	}
}

// copyBidirectionalWithReader copies data bidirectionally using a buffered reader
// for one side to preserve peeked/buffered data during WebSocket upgrades.
func (st *SharedTunnel) copyBidirectionalWithReader(remote net.Conn, local net.Conn, localReader *bufio.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Local (via reader) -> Remote
	go func() {
		defer wg.Done()
		_, err := io.Copy(remote, localReader)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			logger.Warn("Error copying local->remote: %v", err)
		}
		if tcpConn, ok := remote.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	// Remote -> Local
	go func() {
		defer wg.Done()
		_, err := io.Copy(local, remote)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			logger.Warn("Error copying remote->local: %v", err)
		}
		if tcpConn, ok := local.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
}

// resolveLocalAddr converts a local address to a full "host:port" string.
// If addr already contains ":" it is used as-is (already host:port).
// Otherwise "localhost:" is prepended (plain port number case).
func resolveLocalAddr(addr string) string {
	if strings.Contains(addr, ":") {
		return addr
	}
	return "localhost:" + addr
}

// getLocalPortForHost extracts subdomain from host and returns the local port.
func (st *SharedTunnel) getLocalPortForHost(host string) string {
	// Remove port if present
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	// Try exact match first (full hostname)
	for subdomain, port := range st.Tunnels {
		if strings.HasPrefix(host, subdomain+".") || host == subdomain {
			return port
		}
	}

	// Extract subdomain (first part before first dot)
	subdomain := host
	if idx := strings.Index(host, "."); idx != -1 {
		subdomain = host[:idx]
	}

	if port, ok := st.Tunnels[subdomain]; ok {
		return port
	}

	return ""
}

// StartWithReconnect starts the tunnel with automatic reconnection.
func (st *SharedTunnel) StartWithReconnect(ctx context.Context, config *ReconnectConfig) error {
	if config == nil {
		config = DefaultReconnectConfig()
	}

	attempt := 0
	delay := config.InitialDelay

	for {
		select {
		case <-ctx.Done():
			logger.Info("Context cancelled, shutting down tunnel...")
			st.publishStatus("shutdown", "Tunnel shutdown requested")
			return ctx.Err()
		default:
		}

		attempt++
		logger.Info("Connecting to %s...", st.ServerAddr)

		err := st.Start(ctx)
		if err == nil {
			return nil
		}

		// Check if context was cancelled
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if IsAlreadyConnectedError(err) {
			logger.Error("Session conflict: %v", err)
			st.publishStatus("error", fmt.Sprintf("Session conflict: %v", err))
			return err
		}

		logger.Error("Connection failed: %v", err)
		st.publishStatus("reconnecting", fmt.Sprintf("Connection failed, retrying in %v...", delay))

		if config.MaxAttempts > 0 && attempt >= config.MaxAttempts {
			return fmt.Errorf("max reconnection attempts (%d) reached: %v", config.MaxAttempts, err)
		}

		select {
		case <-ctx.Done():
			logger.Info("Tunnel shutdown requested during reconnect wait")
			return ctx.Err()
		case <-time.After(delay):
		}

		delay = time.Duration(float64(delay) * config.Multiplier)
		if delay > config.MaxDelay {
			delay = config.MaxDelay
		}
	}
}

// Shutdown gracefully shuts down the tunnel.
func (st *SharedTunnel) Shutdown(ctx context.Context) error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true

	if st.session != nil {
		st.session.Close()
	}

	for conn := range st.activeConns {
		conn.Close()
	}
	st.mu.Unlock()

	done := make(chan struct{})
	go func() {
		st.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
