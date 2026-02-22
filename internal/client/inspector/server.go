package inspector

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

// HTTPExchange represents a complete HTTP request/response pair
type HTTPExchange struct {
	ID        int64         `json:"id"`
	Request   *HTTPRequest  `json:"request"`
	Response  *HTTPResponse `json:"response,omitempty"`
	Duration  int64         `json:"duration_ms"`
	Timestamp time.Time     `json:"timestamp"`
}

// HTTPRequest captures request details
type HTTPRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Proto   string              `json:"proto"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	Size    int64               `json:"size"`
}

// HTTPResponse captures response details
type HTTPResponse struct {
	Status  int                 `json:"status"`
	Proto   string              `json:"proto"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	Size    int64               `json:"size"`
}

const maxBodySize int64 = 1024 * 1024 // 1MB max body capture

// Server represents the inspector HTTP server with its own state.
type Server struct {
	store     Store
	localPort string
	httpSrv   *http.Server
	addr      string
}

// NewServer creates a new inspector server.
func NewServer(port, localPort string, store Store) *Server {
	if store == nil {
		store = NewInMemoryStore(100)
	}
	return &Server{
		store:     store,
		localPort: localPort,
		addr:      ":" + port,
	}
}

// Start starts the inspector server and blocks until context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	s.setupRoutes(mux)

	s.httpSrv = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	// Handle graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(shutdownCtx)
	}()

	err := s.httpSrv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// StartAsync starts the inspector server in a goroutine.
func (s *Server) StartAsync(ctx context.Context) {
	go s.Start(ctx)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// AddExchange adds an exchange to the server's store.
func (s *Server) AddExchange(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte, duration time.Duration) int64 {
	exchange := HTTPExchange{
		Timestamp: time.Now(),
		Duration:  duration.Milliseconds(),
		Request: &HTTPRequest{
			Method:  req.Method,
			URL:     req.URL.String(),
			Proto:   req.Proto,
			Headers: req.Header,
			Body:    truncateBody(reqBody),
			Size:    int64(len(reqBody)),
		},
	}

	if resp != nil {
		exchange.Response = &HTTPResponse{
			Status:  resp.StatusCode,
			Proto:   resp.Proto,
			Headers: resp.Header,
			Body:    truncateBody(respBody),
			Size:    int64(len(respBody)),
		}
	}

	return s.store.Add(exchange)
}

// Store returns the server's exchange store.
func (s *Server) Store() Store {
	return s.store
}

// SetLocalPort updates the local port for replay functionality.
func (s *Server) SetLocalPort(port string) {
	s.localPort = port
}

func (s *Server) setupRoutes(mux *http.ServeMux) {
	// Serve UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	// List all exchanges
	mux.HandleFunc("/api/exchanges", func(w http.ResponseWriter, r *http.Request) {
		exchanges := s.store.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(exchanges)
	})

	// Get single exchange or replay
	mux.HandleFunc("/api/exchanges/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/api/exchanges/")

		// Handle replay endpoint
		if strings.HasPrefix(idStr, "replay/") {
			s.handleReplay(w, r, strings.TrimPrefix(idStr, "replay/"))
			return
		}

		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid ID", http.StatusBadRequest)
			return
		}

		exchange, ok := s.store.Get(id)
		if !ok {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(exchange)
	})

	// Replay endpoint
	mux.HandleFunc("/api/replay/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleReplay(w, r, strings.TrimPrefix(r.URL.Path, "/api/replay/"))
	})

	// Clear exchanges
	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.store.Clear()
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request, idStr string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	exchange, ok := s.store.Get(id)
	if !ok {
		http.Error(w, "Exchange not found", http.StatusNotFound)
		return
	}

	if s.localPort == "" {
		http.Error(w, "Replay not configured (no local port)", http.StatusInternalServerError)
		return
	}

	// Reconstruct the request
	localAddr := s.localPort
	if !strings.Contains(localAddr, ":") {
		localAddr = "localhost:" + localAddr
	}
	reqURL := "http://" + localAddr + exchange.Request.URL
	req, err := http.NewRequest(exchange.Request.Method, reqURL, bytes.NewReader([]byte(exchange.Request.Body)))
	if err != nil {
		http.Error(w, "Failed to create request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for k, vv := range exchange.Request.Headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	// Execute request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Replay failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  resp.StatusCode,
		"headers": resp.Header,
		"body":    string(respBody),
	})
}

// truncateBody limits body size for storage
func truncateBody(body []byte) string {
	if int64(len(body)) > maxBodySize {
		return string(body[:maxBodySize]) + "\n... (truncated)"
	}
	return string(body)
}

// ============================================================================
// Global state and functions for backward compatibility
// These will be used until CLI is refactored to use Server directly
// ============================================================================

var (
	globalStore Store
	globalMu    sync.RWMutex
	globalPort  string
)

func init() {
	globalStore = NewInMemoryStore(100)
}

// SetLocalPort configures the local port for replay functionality (global).
func SetLocalPort(port string) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalPort = port
}

// AddExchange records a complete HTTP exchange (global).
func AddExchange(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte, duration time.Duration) int64 {
	exchange := HTTPExchange{
		Timestamp: time.Now(),
		Duration:  duration.Milliseconds(),
		Request: &HTTPRequest{
			Method:  req.Method,
			URL:     req.URL.String(),
			Proto:   req.Proto,
			Headers: req.Header,
			Body:    truncateBody(reqBody),
			Size:    int64(len(reqBody)),
		},
	}

	if resp != nil {
		exchange.Response = &HTTPResponse{
			Status:  resp.StatusCode,
			Proto:   resp.Proto,
			Headers: resp.Header,
			Body:    truncateBody(respBody),
			Size:    int64(len(respBody)),
		}
	}

	return globalStore.Add(exchange)
}

// GetExchange retrieves a specific exchange by ID (global).
func GetExchange(id int64) (*HTTPExchange, bool) {
	return globalStore.Get(id)
}

// Start launches the inspector web server (global, legacy).
func Start(port string) {
	mux := http.NewServeMux()

	// Serve UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	// List all exchanges
	mux.HandleFunc("/api/exchanges", func(w http.ResponseWriter, r *http.Request) {
		exchanges := globalStore.List()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(exchanges)
	})

	// Get single exchange
	mux.HandleFunc("/api/exchanges/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/api/exchanges/")

		// Handle replay endpoint
		if strings.HasPrefix(idStr, "replay/") {
			handleGlobalReplay(w, r, strings.TrimPrefix(idStr, "replay/"))
			return
		}

		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid ID", http.StatusBadRequest)
			return
		}

		exchange, ok := GetExchange(id)
		if !ok {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(exchange)
	})

	// Replay endpoint
	mux.HandleFunc("/api/replay/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleGlobalReplay(w, r, strings.TrimPrefix(r.URL.Path, "/api/replay/"))
	})

	go http.ListenAndServe(":"+port, mux)
}

// handleGlobalReplay handles replay using global state.
func handleGlobalReplay(w http.ResponseWriter, r *http.Request, idStr string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	exchange, ok := GetExchange(id)
	if !ok {
		http.Error(w, "Exchange not found", http.StatusNotFound)
		return
	}

	globalMu.RLock()
	port := globalPort
	globalMu.RUnlock()

	if port == "" {
		http.Error(w, "Replay not configured (no local port)", http.StatusInternalServerError)
		return
	}

	// Reconstruct the request
	localAddrGlobal := port
	if !strings.Contains(localAddrGlobal, ":") {
		localAddrGlobal = "localhost:" + localAddrGlobal
	}
	reqURL := "http://" + localAddrGlobal + exchange.Request.URL
	req, err := http.NewRequest(exchange.Request.Method, reqURL, bytes.NewReader([]byte(exchange.Request.Body)))
	if err != nil {
		http.Error(w, "Failed to create request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for k, vv := range exchange.Request.Headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	// Execute request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Replay failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Return response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  resp.StatusCode,
		"headers": resp.Header,
		"body":    string(respBody),
	})
}

// AddRequest is a legacy function for backward compatibility.
func AddRequest(method, host, path string, status int) {
	exchange := HTTPExchange{
		Timestamp: time.Now(),
		Request: &HTTPRequest{
			Method: method,
			URL:    path,
			Headers: map[string][]string{
				"Host": {host},
			},
		},
	}

	if status > 0 {
		exchange.Response = &HTTPResponse{
			Status: status,
		}
	}

	globalStore.Add(exchange)
}
