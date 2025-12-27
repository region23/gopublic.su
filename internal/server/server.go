package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"gopublic/internal/config"
	"gopublic/internal/models"
	"gopublic/internal/sentry"
	"gopublic/internal/storage"
	"gopublic/pkg/protocol"
)

// Server manages the control plane for tunnel connections.
// It handles client authentication, domain binding, and session management.
type Server struct {
	Registry     *TunnelRegistry
	UserSessions *UserSessionRegistry // Tracks active sessions per user
	Port         string
	TLSConfig    *tls.Config
	RootDomain   string // Root domain for FQDN generation

	listener net.Listener
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc

	// MaxConnections limits concurrent connections (0 = unlimited)
	MaxConnections int
	connSem        chan struct{}

	// DailyBandwidthLimit is the daily bandwidth limit per user in bytes
	DailyBandwidthLimit int64
}

// NewServerWithConfig creates a new server with the given configuration.
func NewServerWithConfig(cfg *config.Config, registry *TunnelRegistry, tlsConfig *tls.Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		Registry:            registry,
		UserSessions:        NewUserSessionRegistry(),
		Port:                cfg.ControlPlanePort,
		TLSConfig:           tlsConfig,
		RootDomain:          cfg.Domain,
		ctx:                 ctx,
		cancel:              cancel,
		MaxConnections:      cfg.MaxConnections,
		DailyBandwidthLimit: cfg.DailyBandwidthLimit,
	}
}

// NewServer creates a new server (deprecated, use NewServerWithConfig).
func NewServer(port string, registry *TunnelRegistry, tlsConfig *tls.Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		Registry:       registry,
		UserSessions:   NewUserSessionRegistry(),
		Port:           port,
		TLSConfig:      tlsConfig,
		RootDomain:     os.Getenv("DOMAIN_NAME"), // Fallback for backward compat
		ctx:            ctx,
		cancel:         cancel,
		MaxConnections: 1000,
	}
}

func (s *Server) Start() error {
	var err error

	if s.TLSConfig != nil {
		s.listener, err = tls.Listen("tcp", s.Port, s.TLSConfig)
	} else {
		s.listener, err = net.Listen("tcp", s.Port)
	}

	if err != nil {
		return err
	}

	// Initialize connection semaphore for rate limiting
	if s.MaxConnections > 0 {
		s.connSem = make(chan struct{}, s.MaxConnections)
	}

	log.Printf("Control Plane listening on %s (TLS=%v, MaxConn=%d)", s.Port, s.TLSConfig != nil, s.MaxConnections)

	for {
		// Check if we're shutting down
		select {
		case <-s.ctx.Done():
			log.Println("Control Plane: shutdown signal received, stopping accept loop")
			return nil
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			// Check if this is a shutdown-related error
			if s.ctx.Err() != nil {
				log.Println("Control Plane: listener closed during shutdown")
				return nil
			}

			// Check if it's a temporary error
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				log.Printf("Temporary accept error: %v, retrying...", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Permanent error
			log.Printf("Failed to accept connection: %v", err)
			return err
		}

		// Acquire semaphore slot (rate limiting)
		if s.connSem != nil {
			select {
			case s.connSem <- struct{}{}:
				// Got slot, proceed
			case <-s.ctx.Done():
				conn.Close()
				return nil
			}
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() {
				if s.connSem != nil {
					<-s.connSem // Release semaphore slot
				}
			}()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Panic recovered in handleConnection: %v", r)
				}
			}()
			s.handleConnection(c)
		}(conn)
	}
}

// Shutdown gracefully stops the server.
// It closes the listener, waits for active connections to finish,
// and respects the provided context's deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("Control Plane: initiating shutdown...")

	// Signal all goroutines to stop
	s.cancel()

	// Close listener to stop accepting new connections
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			log.Printf("Error closing listener: %v", err)
		}
	}

	// Wait for active connections with timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("Control Plane: all connections closed gracefully")
		return nil
	case <-ctx.Done():
		log.Println("Control Plane: shutdown timeout, forcing close")
		return ctx.Err()
	}
}

// handleConnection processes a new client connection through the handshake protocol.
func (s *Server) handleConnection(conn net.Conn) {
	log.Printf("New connection from %s", conn.RemoteAddr())

	// 1. Setup yamux session
	session, stream, err := s.setupYamuxSession(conn)
	if err != nil {
		sentry.CaptureErrorf(err, "Session setup failed for %s", conn.RemoteAddr())
		return
	}

	// Create a single decoder for the entire handshake to avoid buffering issues
	decoder := json.NewDecoder(stream)

	// 2. Authenticate client
	user, force, err := s.authenticate(decoder, stream, conn.RemoteAddr().String())
	if err != nil {
		sentry.CaptureErrorf(err, "Authentication failed for %s", conn.RemoteAddr())
		session.Close()
		return
	}

	// 3. Check for existing session
	if existingSession, exists := s.UserSessions.GetSession(user.ID); exists {
		if !force {
			// Reject connection - user already has active session
			log.Printf("User %d already connected, rejecting new connection (use force=true to override)", user.ID)
			s.sendErrorWithCode(stream, "You already have an active tunnel session. Use --force to disconnect the existing session.", protocol.ErrorCodeAlreadyConnected)
			session.Close()
			return
		}

		// Force mode: disconnect old session
		log.Printf("Force disconnect: closing existing session for user %d", user.ID)
		// Unregister old domains first
		for _, domain := range existingSession.Domains {
			s.Registry.Unregister(domain)
		}
		existingSession.Session.Close()
		s.UserSessions.Unregister(user.ID)
	}

	// 4. Process tunnel request and bind domains
	boundDomains, err := s.processTunnelRequest(decoder, stream, session, user, conn.RemoteAddr().String())
	if err != nil {
		sentry.CaptureErrorf(err, "Tunnel request failed for %s", conn.RemoteAddr())
		session.Close()
		return
	}

	// 5. Register user session
	s.UserSessions.Register(user.ID, session, boundDomains)

	// 6. Send success response
	if err := s.sendSuccessResponse(stream, boundDomains, user.ID); err != nil {
		sentry.CaptureErrorf(err, "Failed to send success response to %s", conn.RemoteAddr())
	}
	log.Printf("Handshake complete for %s. Bound domains: %v", conn.RemoteAddr(), boundDomains)

	// 7. Monitor session for cleanup
	s.monitorSession(session, user.ID, boundDomains)
}

// Handshake timeout for server-side operations
const handshakeTimeout = 10 * time.Second

// setupYamuxSession creates a yamux session and accepts the handshake stream.
func (s *Server) setupYamuxSession(conn net.Conn) (*yamux.Session, net.Conn, error) {
	// Set initial deadline for yamux setup
	conn.SetDeadline(time.Now().Add(handshakeTimeout))

	session, err := yamux.Server(conn, nil)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	log.Printf("Yamux session established for %s", conn.RemoteAddr())

	stream, err := session.Accept()
	if err != nil {
		session.Close()
		return nil, nil, err
	}
	log.Printf("Handshake stream accepted from %s", conn.RemoteAddr())

	// Clear deadline on the underlying connection after yamux is established
	conn.SetDeadline(time.Time{})

	return session, stream, nil
}

// authenticate validates the client's token and returns the user and force flag.
func (s *Server) authenticate(decoder *json.Decoder, stream net.Conn, remoteAddr string) (*models.User, bool, error) {
	// Set read deadline for auth request
	stream.SetReadDeadline(time.Now().Add(handshakeTimeout))
	defer stream.SetReadDeadline(time.Time{}) // Clear deadline after auth

	var authReq protocol.AuthRequest
	if err := decoder.Decode(&authReq); err != nil {
		return nil, false, err
	}
	log.Printf("Auth request received from %s (force=%v)", remoteAddr, authReq.Force)

	user, err := storage.ValidateToken(authReq.Token)
	if err != nil {
		s.sendErrorWithCode(stream, "Invalid Token", protocol.ErrorCodeInvalidToken)
		return nil, false, err
	}
	log.Printf("User %s authenticated (ID: %d)", user.Username, user.ID)

	return user, authReq.Force, nil
}

// processTunnelRequest handles the tunnel request and binds domains.
func (s *Server) processTunnelRequest(decoder *json.Decoder, stream net.Conn, session *yamux.Session, user *models.User, remoteAddr string) ([]string, error) {
	// Set read deadline for tunnel request
	stream.SetReadDeadline(time.Now().Add(handshakeTimeout))

	var tunnelReq protocol.TunnelRequest
	if err := decoder.Decode(&tunnelReq); err != nil {
		return nil, err
	}
	log.Printf("Tunnel request received from %s for %d domains", remoteAddr, len(tunnelReq.RequestedDomains))

	// Clear read deadline before database operations
	stream.SetReadDeadline(time.Time{})

	// If no domains requested, get all user domains
	requestedDomains := tunnelReq.RequestedDomains
	if len(requestedDomains) == 0 {
		userDomains, err := storage.GetUserDomains(user.ID)
		if err != nil {
			s.sendError(stream, "Failed to retrieve user domains")
			return nil, err
		}
		log.Printf("Client requested all domains. Found %d domains in DB for user %d", len(userDomains), user.ID)
		for _, d := range userDomains {
			requestedDomains = append(requestedDomains, d.Name)
		}
	}

	// Bind domains
	boundDomains := s.bindDomains(session, user.ID, requestedDomains)

	if len(boundDomains) == 0 {
		s.sendError(stream, "No valid domains requested or authorized")
		return nil, errors.New("no domains bound")
	}

	return boundDomains, nil
}

// bindDomains validates ownership and registers domains with the session.
func (s *Server) bindDomains(session *yamux.Session, userID uint, requestedDomains []string) []string {
	var boundDomains []string

	for _, name := range requestedDomains {
		log.Printf("Processing domain bind: %s (User: %d)", name, userID)

		isOwner, err := storage.ValidateDomainOwnership(name, userID)
		if err != nil {
			log.Printf("Domain ownership check error for %s: %v", name, err)
			continue
		}

		if !isOwner {
			log.Printf("Domain ownership validation failed: %s (User: %d)", name, userID)
			continue
		}

		// Register FQDN if rootDomain is set, otherwise just name (local dev)
		regName := name
		if s.RootDomain != "" {
			regName = name + "." + s.RootDomain
		}

		s.Registry.Register(regName, session, userID)
		boundDomains = append(boundDomains, regName)
		log.Printf("Successfully bound domain %s for user %d", regName, userID)
	}

	return boundDomains
}

// sendSuccessResponse sends the handshake success response to the client.
func (s *Server) sendSuccessResponse(stream net.Conn, boundDomains []string, userID uint) error {
	// Fetch bandwidth statistics for the user
	bandwidthToday, _ := storage.GetUserBandwidthToday(userID)
	bandwidthTotal, _ := storage.GetUserTotalBandwidth(userID)

	resp := protocol.InitResponse{
		Success:      true,
		BoundDomains: boundDomains,
		ServerStats: &protocol.ServerStats{
			BandwidthToday: bandwidthToday,
			BandwidthTotal: bandwidthTotal,
			BandwidthLimit: s.DailyBandwidthLimit,
		},
	}
	return json.NewEncoder(stream).Encode(resp)
}

// monitorSession watches for session close and cleans up domain registrations.
func (s *Server) monitorSession(session *yamux.Session, userID uint, boundDomains []string) {
	go func() {
		<-session.CloseChan()
		log.Printf("Session closed for user %d. Cleaning up domains.", userID)
		for _, d := range boundDomains {
			s.Registry.Unregister(d)
		}
		s.UserSessions.Unregister(userID)
	}()
}

func (s *Server) sendError(stream net.Conn, msg string) {
	resp := protocol.InitResponse{
		Success: false,
		Error:   msg,
	}
	json.NewEncoder(stream).Encode(resp)
}

func (s *Server) sendErrorWithCode(stream net.Conn, msg string, code protocol.ErrorCode) {
	resp := protocol.InitResponse{
		Success:   false,
		Error:     msg,
		ErrorCode: code,
	}
	json.NewEncoder(stream).Encode(resp)
}
