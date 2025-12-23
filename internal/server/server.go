package server

import (
	"crypto/tls"
	"encoding/json"
	"gopublic/internal/storage"
	"gopublic/pkg/protocol"
	"log"
	"net"
	"os"

	"github.com/hashicorp/yamux"
)

type Server struct {
	Registry  *TunnelRegistry
	Port      string
	TLSConfig *tls.Config
}

func NewServer(port string, registry *TunnelRegistry, tlsConfig *tls.Config) *Server {
	return &Server{
		Registry:  registry,
		Port:      port,
		TLSConfig: tlsConfig,
	}
}

func (s *Server) Start() error {
	var listener net.Listener
	var err error

	if s.TLSConfig != nil {
		listener, err = tls.Listen("tcp", s.Port, s.TLSConfig)
	} else {
		listener, err = net.Listen("tcp", s.Port)
	}

	if err != nil {
		return err
	}
	log.Printf("Control Plane listening on %s (TLS=%v)", s.Port, s.TLSConfig != nil)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}
		go s.handleConnection(conn)
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
		userDomains := storage.GetUserDomains(user.ID)
		log.Printf("Client requested all domains. Found %d domains in DB for user %d", len(userDomains), user.ID)
		for _, d := range userDomains {
			tunnelReq.RequestedDomains = append(tunnelReq.RequestedDomains, d.Name)
		}
	}

	for _, name := range tunnelReq.RequestedDomains {
		log.Printf("Processing domain bind: %s (User: %d)", name, user.ID)
		if storage.ValidateDomainOwnership(name, user.ID) {
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
