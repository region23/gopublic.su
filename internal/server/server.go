package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"gopublic/internal/storage"
	"gopublic/pkg/protocol"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Server manages the control plane for tunnel connections.
// It handles client authentication, domain binding, and session management.
type Server struct {
	Registry  *TunnelRegistry
	Port      string
	TLSConfig *tls.Config

	listener net.Listener
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc

	// MaxConnections limits concurrent connections (0 = unlimited)
	MaxConnections int
	connSem        chan struct{}
}

func NewServer(port string, registry *TunnelRegistry, tlsConfig *tls.Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		Registry:       registry,
		Port:           port,
		TLSConfig:      tlsConfig,
		ctx:            ctx,
		cancel:         cancel,
		MaxConnections: 1000, // Default limit
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

func (s *Server) handleConnection(conn net.Conn) {
	log.Printf("New connection from %s", conn.RemoteAddr())
	// Wrap connection with Yamux
	session, err := yamux.Server(conn, nil)
	if err != nil {
		log.Printf("Failed to create yamux session for %s: %v", conn.RemoteAddr(), err)
		conn.Close()
		return
	}
	log.Printf("Yamux session established for %s", conn.RemoteAddr())

	// Accept the first stream for Handshake
	stream, err := session.Accept()
	if err != nil {
		log.Printf("Failed to accept handshake stream from %s: %v", conn.RemoteAddr(), err)
		session.Close()
		return
	}
	log.Printf("Handshake stream accepted from %s", conn.RemoteAddr())

	// Perform Handshake
	decoder := json.NewDecoder(stream)

	// 1. Auth
	var authReq protocol.AuthRequest
	if err := decoder.Decode(&authReq); err != nil {
		log.Printf("Failed to decode auth request from %s: %v", conn.RemoteAddr(), err)
		session.Close()
		return
	}
	log.Printf("Auth request received from %s", conn.RemoteAddr())

	user, err := storage.ValidateToken(authReq.Token)
	if err != nil {
		log.Printf("Token validation failed for %s: %v", conn.RemoteAddr(), err)
		s.sendError(stream, "Invalid Token")
		session.Close()
		return
	}
	log.Printf("User %s authenticated (ID: %d)", user.Username, user.ID)

	// 2. Tunnel Request
	var tunnelReq protocol.TunnelRequest
	if err := decoder.Decode(&tunnelReq); err != nil {
		log.Printf("Failed to decode tunnel request from %s: %v", conn.RemoteAddr(), err)
		session.Close()
		return
	}
	log.Printf("Tunnel request received from %s for %d domains", conn.RemoteAddr(), len(tunnelReq.RequestedDomains))

	var boundDomains []string
	rootDomain := os.Getenv("DOMAIN_NAME")

	// If no domains requested, bind ALL user domains
	if len(tunnelReq.RequestedDomains) == 0 {
		userDomains, err := storage.GetUserDomains(user.ID)
		if err != nil {
			log.Printf("Failed to get user domains for %s: %v", conn.RemoteAddr(), err)
			s.sendError(stream, "Failed to retrieve user domains")
			session.Close()
			return
		}
		log.Printf("Client requested all domains. Found %d domains in DB for user %d", len(userDomains), user.ID)
		for _, d := range userDomains {
			tunnelReq.RequestedDomains = append(tunnelReq.RequestedDomains, d.Name)
		}
	}

	for _, name := range tunnelReq.RequestedDomains {
		log.Printf("Processing domain bind: %s (User: %d)", name, user.ID)
		isOwner, err := storage.ValidateDomainOwnership(name, user.ID)
		if err != nil {
			log.Printf("Domain ownership check error for %s: %v", name, err)
			continue
		}
		if isOwner {
			// Register FQDN if rootDomain is set, otherwise just name (local dev)
			regName := name
			if rootDomain != "" {
				regName = name + "." + rootDomain
			}

			s.Registry.Register(regName, session)
			boundDomains = append(boundDomains, regName)
			log.Printf("Successfully bound domain %s for user %d", regName, user.ID)
		} else {
			log.Printf("Domain ownership validation failed: %s (User: %d)", name, user.ID)
		}
	}

	if len(boundDomains) == 0 {
		log.Printf("No domains bound for %s. Closing session.", conn.RemoteAddr())
		s.sendError(stream, "No valid domains requested or authorized")
		session.Close()
		return
	}

	// 3. Success Response
	resp := protocol.InitResponse{
		Success:      true,
		BoundDomains: boundDomains,
	}
	if err := json.NewEncoder(stream).Encode(resp); err != nil {
		log.Printf("Failed to send success response to %s: %v", conn.RemoteAddr(), err)
	}
	log.Printf("Handshake complete for %s. Bound domains: %v", conn.RemoteAddr(), boundDomains)

	// Keep session alive. Monitor for close to cleanup.
	go func() {
		<-session.CloseChan()
		log.Printf("Session closed for user %d. Cleaning up domains.", user.ID)
		for _, d := range boundDomains {
			s.Registry.Unregister(d)
		}
	}()
}

func (s *Server) sendError(stream net.Conn, msg string) {
	resp := protocol.InitResponse{
		Success: false,
		Error:   msg,
	}
	json.NewEncoder(stream).Encode(resp)
}
