package ingress

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"

	"gopublic/internal/config"
	"gopublic/internal/dashboard"
	"gopublic/internal/middleware"
	"gopublic/internal/sentry"
	"gopublic/internal/server"
	"gopublic/internal/storage"
	"gopublic/internal/version"
)

// hostPattern validates hostnames (RFC 1123 compliant + localhost).
// Allows alphanumeric, hyphens, dots; max 253 chars; labels max 63 chars.
var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?)*$`)

var errBandwidthLimitExceeded = errors.New("daily bandwidth limit exceeded")

type Ingress struct {
	Registry            *server.TunnelRegistry
	DashHandler         *dashboard.Handler
	Port                string
	RootDomain          string // Root domain for routing
	ProjectName         string // Project name for branding
	IsSecure            bool   // Whether running in secure mode
	GitHubRepo          string // GitHub repo for client downloads (e.g., "username/gopublic")
	DailyBandwidthLimit int64  // Daily bandwidth limit per user in bytes (0 = unlimited)
	SentryEnabled       bool   // Whether Sentry is configured

	quotaNotifyMu   sync.Mutex
	quotaNotifiedAt map[uint]time.Time
}

// NewIngressWithConfig creates a new ingress with the given configuration.
func NewIngressWithConfig(cfg *config.Config, registry *server.TunnelRegistry, dash *dashboard.Handler) *Ingress {
	return &Ingress{
		Registry:            registry,
		DashHandler:         dash,
		Port:                cfg.IngressPort(),
		RootDomain:          cfg.Domain,
		ProjectName:         cfg.ProjectName,
		IsSecure:            cfg.IsSecure(),
		GitHubRepo:          cfg.GitHubRepo,
		DailyBandwidthLimit: cfg.DailyBandwidthLimit,
		SentryEnabled:       cfg.HasSentry(),
		quotaNotifiedAt:     make(map[uint]time.Time),
	}
}

// NewIngress creates a new ingress (deprecated, use NewIngressWithConfig).
func NewIngress(port string, registry *server.TunnelRegistry, dash *dashboard.Handler) *Ingress {
	projectName := os.Getenv("PROJECT_NAME")
	if projectName == "" {
		projectName = "Go Public"
	}
	return &Ingress{
		Registry:    registry,
		DashHandler: dash,
		Port:        port,
		RootDomain:  os.Getenv("DOMAIN_NAME"),
		ProjectName: projectName,
		IsSecure:    false,
		quotaNotifiedAt: make(map[uint]time.Time),
	}
}

func (i *Ingress) maybeNotifyBandwidthExceeded(entry *server.TunnelEntry) {
	if entry == nil || entry.Session == nil {
		return
	}
	if i.DailyBandwidthLimit <= 0 {
		return
	}
	if entry.BandwidthExempt {
		return
	}

	shouldSend := false
	now := time.Now()
	i.quotaNotifyMu.Lock()
	last, ok := i.quotaNotifiedAt[entry.UserID]
	if !ok || now.Sub(last) > time.Minute {
		i.quotaNotifiedAt[entry.UserID] = now
		shouldSend = true
	}
	i.quotaNotifyMu.Unlock()
	if !shouldSend {
		return
	}

	stream, err := entry.Session.Open()
	if err != nil {
		return
	}
	defer stream.Close()

	_, _ = fmt.Fprintf(stream, "GET /__gopublic/control/bandwidth_exceeded HTTP/1.1\r\nHost: gopublic-control\r\nX-GoPublic-Control: bandwidth_exceeded\r\nRetry-After: 86400\r\n\r\n")
}

func (i *Ingress) Handler() http.Handler {
	// Set Gin to release mode
	gin.SetMode(gin.ReleaseMode)

	r := gin.Default()

	// Add Sentry middleware if configured (must be before other middleware to capture panics)
	if i.SentryEnabled {
		r.Use(sentrygin.New(sentrygin.Options{
			Repanic: true, // Let gin.Default's Recovery handle the response
		}))
	}

	// Add CSRF middleware for dashboard routes
	r.Use(middleware.SetCSRFToken(&middleware.CSRFConfig{Secure: i.IsSecure}))

	// Register Dashboard Routes (will be handled via Host matching in middleware or here)
	// Actually, `dashboard.Handler.RegisterRoutes` registers routes on `r`.
	// But `r` catches everything.
	// We need to route based on Host.
	// Strategy:
	// 1. Register Dashboard routes on a sub-group or let DashHandler register what it needs.
	//    BUT checking `handleRequest` (the catch-all) logic in previous steps shows it handles "app.domain".
	//    The logic in `handleRequest` does: `if host == "app."+domain { ... }`
	//    We should delegate that to `i.DashHandler`.

	// Let's modify handleRequest to call DashHandler if host matches.
	// BUT `DashHandler` is designed as a set of Gin Routes (GET /login, GET /).
	// We can't easily jump from a catch-all handler into defined Gin routes unless we register them.

	// Better Strategy:
	// Register Dashboard routes for SPECIFIC HOST explicitly? Gin doesn't support Host routing easily without middleware.

	// Alternate Strategy (Simplest for now):
	// Register Dashboard handlers directly in the main Engine, but use a middleware to enforce Host "app.domain".
	// Or, just use the `handleRequest` to start the dashboard engine? No.

	// Let's stick to the current plan:
	// `handleRequest` is the NoRoute handler.
	// If `handleRequest` detects `app.domain`, it needs to serve the dashboard.
	// Since we already have the `DashHandler` which registers routes like `/`, `/login`.
	// If we register those routes on `r`, they will conflict with tunnel routing if we are not careful (tunnel uses subdomains).
	// But `/login` on a tunnel domain `foo.example.com` should just be proxied.

	// Solution:
	// Use Middleware to check Host.
	// IF host == app.domain -> Continue to Gin Routes.
	// IF host != app.domain -> Abort Gin Routes and go to Tunnel logic (which is likely the NoRoute handler?)

	// Let's register Dashboard routes normally.
	// And add a Middleware at the top that says:
	// "If Host != app.domain, Skip/Next" -> But Gin matches path first.

	// OK, looking at `dashboard/handler.go`, it does `r.GET("/", ...)`.
	// If I register that, then `http://tunnel.com/` will hit the dashboard handler if I'm not careful.
	// I need a middleware that says: "If this route is matched, ensure Host is app.domain".

	// Let's do this:
	// Pass `r` to `i.DashHandler.RegisterRoutes(r)`.
	// But inside `RegisterRoutes`, wraps handlers with `RequireDashboardDomain`.

	// Or simply:
	// In `handleRequest` (which captures 404s/unknowns), we handle Tunnels.
	// For Dashboard, we explicitly register routes.
	// But we need to ensure those routes ONLY match `app.domain`.

	// Load Templates (delegated to Dashboard handler helper)
	// We need to ensure Gin engine has templates loaded so c.HTML works in manual dispatch.
	if err := i.DashHandler.LoadTemplates(r); err != nil {
		log.Printf("Failed to load templates: %v", err)
	}

	// Catch-all handler for Tunnels (and Landing Page)
	r.NoRoute(i.handleRequest)
	return r
}

func (i *Ingress) Start() error {
	log.Printf("Public Ingress listening on %s (HTTP)", i.Port)
	return http.ListenAndServe(i.Port, i.Handler())
}

// handleRequest routes incoming requests to the appropriate handler.
func (i *Ingress) handleRequest(c *gin.Context) {
	host, valid := i.parseAndValidateHost(c.Request.Host)
	if !valid {
		c.String(http.StatusBadRequest, "Invalid host header")
		return
	}

	// Route based on host
	switch {
	case i.isLandingPage(host):
		i.serveLandingPage(c)
	case i.isDashboardHost(host):
		i.serveDashboard(c)
	default:
		i.proxyToTunnel(c, host)
	}
}

// parseAndValidateHost extracts and validates the hostname.
// Returns the hostname and whether it's valid.
func (i *Ingress) parseAndValidateHost(host string) (string, bool) {
	// Remove port if present
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	// Empty host is invalid
	if host == "" {
		return "", false
	}

	// Check max length (RFC 1123)
	if len(host) > 253 {
		return "", false
	}

	// Validate format (alphanumeric, hyphens, dots only)
	if !hostPattern.MatchString(host) {
		return "", false
	}

	return strings.ToLower(host), true
}

// parseHost extracts the hostname without port (deprecated, use parseAndValidateHost).
func (i *Ingress) parseHost(host string) string {
	if idx := strings.Index(host, ":"); idx != -1 {
		return host[:idx]
	}
	return host
}

// isLocalDev returns true if running in local development mode.
func (i *Ingress) isLocalDev() bool {
	return i.RootDomain == "" || i.RootDomain == "127.0.0.1" || i.RootDomain == "localhost"
}

// isLandingPage returns true if the host matches the root domain (non-dev mode).
func (i *Ingress) isLandingPage(host string) bool {
	return !i.isLocalDev() && i.RootDomain != "" && host == i.RootDomain
}

// isDashboardHost returns true if the host should serve the dashboard.
func (i *Ingress) isDashboardHost(host string) bool {
	if i.RootDomain == "" {
		return false
	}
	if i.isLocalDev() {
		return host == i.RootDomain
	}
	return host == "app."+i.RootDomain
}

// serveLandingPage renders the public landing page or install scripts.
func (i *Ingress) serveLandingPage(c *gin.Context) {
	switch c.Request.URL.Path {
	case "/install.sh":
		i.serveInstallSh(c)
	case "/install.ps1":
		i.serveInstallPs1(c)
	case "/terms":
		i.DashHandler.Terms(c)
	case "/abuse":
		if c.Request.Method == http.MethodPost {
			i.DashHandler.SubmitAbuseReport(c)
		} else {
			i.DashHandler.AbuseForm(c)
		}
	default:
		scheme := "http"
		if i.IsSecure {
			scheme = "https"
		}
		c.HTML(http.StatusOK, "landing.html", gin.H{
			"ProjectName":  i.ProjectName,
			"RootDomain":   i.RootDomain,
			"DashboardURL": scheme + "://app." + i.RootDomain,
			"GitHubRepo":   i.GitHubRepo,
			"Version":      version.Version,
		})
	}
}

// serveInstallSh serves the bash install script for macOS/Linux.
func (i *Ingress) serveInstallSh(c *gin.Context) {
	if i.GitHubRepo == "" {
		c.String(http.StatusNotFound, "Install script not available: GITHUB_REPO not configured")
		return
	}

	script := `#!/bin/sh
set -e

REPO="` + i.GitHubRepo + `"
BINARY_NAME="gopublic"
# Install to ~/.local/bin by default (no sudo required, auto-update friendly)
INSTALL_DIR="${GOPUBLIC_INSTALL_DIR:-$HOME/.local/bin}"

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux) OS="linux" ;;
  darwin) OS="macos" ;;
  *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

BINARY="${BINARY_NAME}-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/latest/download/${BINARY}"

# Use temp directory
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

echo "Downloading ${BINARY}..."
curl -fsSL "$URL" -o "$TMPDIR/$BINARY_NAME"
chmod +x "$TMPDIR/$BINARY_NAME"

# Create install directory if needed
mkdir -p "$INSTALL_DIR"

# Install
mv "$TMPDIR/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"
echo ""
echo "Installed: $INSTALL_DIR/$BINARY_NAME"

# Check if install dir is in PATH
case ":$PATH:" in
  *":$INSTALL_DIR:"*)
    echo ""
    echo "Run 'gopublic --help' to get started."
    ;;
  *)
    echo ""
    echo "NOTE: $INSTALL_DIR is not in your PATH."
    echo ""
    SHELL_NAME=$(basename "$SHELL")
    case "$SHELL_NAME" in
      zsh)
        echo "Add it by running:"
        echo "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
        ;;
      bash)
        echo "Add it by running:"
        echo "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc && source ~/.bashrc"
        ;;
      *)
        echo "Add it to your shell config:"
        echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
        ;;
    esac
    echo ""
    echo "Or run directly: $INSTALL_DIR/$BINARY_NAME --help"
    ;;
esac
`
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, script)
}

// serveInstallPs1 serves the PowerShell install script for Windows.
func (i *Ingress) serveInstallPs1(c *gin.Context) {
	if i.GitHubRepo == "" {
		c.String(http.StatusNotFound, "Install script not available: GITHUB_REPO not configured")
		return
	}

	script := `$ErrorActionPreference = "Stop"
$repo = "` + i.GitHubRepo + `"
$url = "https://github.com/$repo/releases/latest/download/gopublic-windows-amd64.exe"

Write-Host "Downloading gopublic..."
Invoke-WebRequest -Uri $url -OutFile "gopublic.exe"
Write-Host ""
Write-Host "Done! Downloaded: .\gopublic.exe"
`
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, script)
}

// serveDashboard routes requests to dashboard handlers.
func (i *Ingress) serveDashboard(c *gin.Context) {
	switch c.Request.URL.Path {
	case "/":
		i.DashHandler.Index(c)
	case "/login":
		i.DashHandler.Login(c)
	case "/auth/telegram":
		i.DashHandler.TelegramCallback(c)
	case "/logout":
		i.DashHandler.Logout(c)
	case "/terms":
		i.DashHandler.Terms(c)
	case "/abuse":
		if c.Request.Method == http.MethodPost {
			i.DashHandler.SubmitAbuseReport(c)
		} else {
			i.DashHandler.AbuseForm(c)
		}
	case "/api/regenerate-token":
		if c.Request.Method == http.MethodPost {
			i.DashHandler.RegenerateToken(c)
		} else {
			c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	case "/api/accept-terms":
		if c.Request.Method == http.MethodPost {
			i.DashHandler.AcceptTerms(c)
		} else {
			c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	case "/auth/yandex":
		i.DashHandler.YandexAuth(c)
	case "/auth/yandex/callback":
		i.DashHandler.YandexCallback(c)
	case "/auth/yandex/suggest/token":
		i.DashHandler.YandexTokenPage(c)
	case "/auth/yandex/token":
		if c.Request.Method == http.MethodPost {
			i.DashHandler.YandexTokenAuth(c)
		} else {
			c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	case "/link/telegram":
		i.DashHandler.LinkTelegram(c)
	case "/auth/telegram/link":
		i.DashHandler.TelegramLinkCallback(c)
	case "/api/telegram-auth/init":
		if c.Request.Method == http.MethodGet {
			i.DashHandler.InitTelegramAuth(c)
		} else {
			c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	case "/api/telegram-auth/poll":
		if c.Request.Method == http.MethodGet {
			i.DashHandler.PollTelegramAuth(c)
		} else {
			c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	case "/metrics":
		i.DashHandler.Metrics(c)
	default:
		// Serve avatar files
		if strings.HasPrefix(c.Request.URL.Path, "/avatars/") {
			i.DashHandler.ServeAvatar(c)
			return
		}
		c.String(http.StatusNotFound, "Not Found")
	}
}

// proxyToTunnel forwards the request to a tunnel client.
func (i *Ingress) proxyToTunnel(c *gin.Context, host string) {
	// Look up tunnel entry (includes user ID)
	entry, ok := i.Registry.GetEntry(host)
	if !ok {
		c.String(http.StatusNotFound, "Tunnel not found for host: %s", host)
		return
	}

	// Capture request size (we need this before opening the stream so we can enforce the limit).
	var reqBuf bytes.Buffer
	if err := c.Request.Write(&reqBuf); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to serialize request")
		c.Status(http.StatusBadGateway)
		return
	}
	requestBytes := int64(reqBuf.Len())

	consume := func(bytes int64) (bool, error) {
		if entry.BandwidthExempt {
			return true, nil
		}
		if i.DailyBandwidthLimit <= 0 || bytes <= 0 {
			return true, nil
		}
		allowed, _, err := storage.ConsumeUserBandwidthWithinLimit(entry.UserID, bytes, i.DailyBandwidthLimit)
		if err != nil {
			log.Printf("Failed to consume bandwidth for user %d: %v", entry.UserID, err)
			// Fail-open on DB errors
			return true, nil
		}
		return allowed, nil
	}

	// Reserve quota for request bytes before opening the tunnel stream.
	if !entry.BandwidthExempt && i.DailyBandwidthLimit > 0 && requestBytes > 0 {
		allowed, _ := consume(requestBytes)
		if !allowed {
			i.maybeNotifyBandwidthExceeded(entry)
			c.Header("Retry-After", "86400") // 24 hours
			c.String(http.StatusTooManyRequests, "Daily bandwidth limit exceeded. Please try again tomorrow.")
			return
		}
	}

	// Open stream to tunnel client
	stream, err := entry.Session.Open()
	if err != nil {
		sentry.CaptureErrorWithContextf(c, err, "Failed to open stream for host %s", host)
		c.String(http.StatusBadGateway, "Failed to connect to tunnel client")
		return
	}
	defer stream.Close()

	// Forward request to tunnel
	if _, err := stream.Write(reqBuf.Bytes()); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to write request to stream")
		c.Status(http.StatusBadGateway)
		return
	}

	// Check if this is an upgrade request (WebSocket, h2c, etc.)
	isUpgrade := isUpgradeRequest(c.Request)

	// Note: bandwidth is consumed during streaming; we don't need to count response bytes here.

	if isUpgrade {
		// For Upgrade requests (WebSocket, h2c, etc.) we must not parse or rewrite the
		// upstream response/body because that can corrupt framing. Instead, we hijack
		// the client connection and tunnel raw bytes bidirectionally.
		hijacker, ok := c.Writer.(http.Hijacker)
		if !ok {
			sentry.CaptureErrorWithContext(c, errors.New("response writer doesn't support hijacking"), "Cannot hijack connection for upgrade")
			c.Status(http.StatusBadGateway)
			return
		}

		clientConn, rw, err := hijacker.Hijack()
		if err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to hijack connection")
			c.Status(http.StatusBadGateway)
			return
		}
		defer clientConn.Close()

		// Ensure any buffered data from the HTTP server is flushed.
		if rw != nil {
			_ = rw.Writer.Flush()
		}

		closeOnce := sync.Once{}
		closeAll := func() {
			closeOnce.Do(func() {
				_ = clientConn.Close()
				_ = stream.Close()
			})
		}

		// Stream -> Client
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			cw := &bandwidthChargingWriter{w: clientConn, consume: func(b int64) (bool, error) {
				allowed, err := consume(b)
				if !allowed {
					i.maybeNotifyBandwidthExceeded(entry)
				}
				return allowed, err
			}, onLimit: closeAll}
			_, _ = io.Copy(cw, bufio.NewReader(stream))
			closeAll()
		}()

		// Client -> Stream (use rw.Reader to include any buffered bytes)
		go func() {
			defer wg.Done()
			clientReader := io.Reader(clientConn)
			if rw != nil {
				clientReader = rw.Reader
			}
			cw := &bandwidthChargingWriter{w: stream, consume: func(b int64) (bool, error) {
				allowed, err := consume(b)
				if !allowed {
					i.maybeNotifyBandwidthExceeded(entry)
				}
				return allowed, err
			}, onLimit: closeAll}
			_, _ = io.Copy(cw, clientReader)
			closeAll()
		}()

		wg.Wait()
		return
	} else {
		// Normal HTTP request/response - existing behavior
		resp, err := http.ReadResponse(bufio.NewReader(stream), c.Request)
		if err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to read response from stream")
			c.Status(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy headers
		for k, vv := range resp.Header {
			for _, v := range vv {
				c.Writer.Header().Add(k, v)
			}
		}

		// Write status and body, counting response bytes
		c.Status(resp.StatusCode)
		closeOnce := sync.Once{}
		closeUpstream := func() {
			closeOnce.Do(func() {
				_ = resp.Body.Close()
				_ = stream.Close()
			})
		}
		cw := &bandwidthChargingWriter{w: c.Writer, consume: func(b int64) (bool, error) {
			allowed, err := consume(b)
			if !allowed {
				i.maybeNotifyBandwidthExceeded(entry)
			}
			return allowed, err
		}, onLimit: closeUpstream}
		_, _ = io.Copy(cw, resp.Body)
	}
}

// isUpgradeRequest checks if the HTTP request is attempting a protocol upgrade
// (WebSocket, h2c, etc.) by examining the Connection header.
func isUpgradeRequest(req *http.Request) bool {
	return strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// copyBidirectionalWithReader copies data bidirectionally between a client connection
// and a stream, using a buffered reader for the stream side to preserve peeked data.
// Returns total bytes transferred in both directions.
func copyBidirectionalWithReader(client net.Conn, stream net.Conn, streamReader *bufio.Reader) int64 {
	var wg sync.WaitGroup
	var totalBytes atomic.Int64

	// Stream (via reader) -> Client
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, err := io.Copy(client, streamReader)
		totalBytes.Add(n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Printf("Error copying stream->client: %v", err)
		}
		// Signal EOF to client
		if tcpConn, ok := client.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	// Client -> Stream
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, err := io.Copy(stream, client)
		totalBytes.Add(n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Printf("Error copying client->stream: %v", err)
		}
		// Signal EOF to stream
		if tcpConn, ok := stream.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	return totalBytes.Load()
}

type bandwidthLimitedWriter struct {
	w         io.Writer
	remaining *atomic.Int64
	onLimit   func()
}

func (lw *bandwidthLimitedWriter) Write(p []byte) (int, error) {
	writtenTotal := 0
	for len(p) > 0 {
		rem := lw.remaining.Load()
		if rem <= 0 {
			if lw.onLimit != nil {
				lw.onLimit()
			}
			if writtenTotal > 0 {
				return writtenTotal, errBandwidthLimitExceeded
			}
			return 0, errBandwidthLimitExceeded
		}

		toWrite := int64(len(p))
		if toWrite > rem {
			toWrite = rem
		}

		n, err := lw.w.Write(p[:toWrite])
		if n > 0 {
			lw.remaining.Add(-int64(n))
			p = p[n:]
			writtenTotal += n
		}
		if err != nil {
			return writtenTotal, err
		}
		if int64(n) < toWrite {
			return writtenTotal, io.ErrShortWrite
		}
		if toWrite == rem {
			if lw.onLimit != nil {
				lw.onLimit()
			}
			return writtenTotal, errBandwidthLimitExceeded
		}
	}
	return writtenTotal, nil
}

func copyBidirectionalWithReaderLimited(client net.Conn, stream net.Conn, streamReader *bufio.Reader, remaining *atomic.Int64) int64 {
	var wg sync.WaitGroup
	var totalBytes atomic.Int64
	var closeOnce sync.Once

	closeAll := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = stream.Close()
		})
	}

	// Stream (via reader) -> Client
	wg.Add(1)
	go func() {
		defer wg.Done()
		lw := &bandwidthLimitedWriter{w: client, remaining: remaining, onLimit: closeAll}
		n, err := io.Copy(lw, streamReader)
		totalBytes.Add(n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errBandwidthLimitExceeded) {
			log.Printf("Error copying stream->client: %v", err)
		}
		if errors.Is(err, errBandwidthLimitExceeded) {
			closeAll()
		}
		// Signal EOF to client
		if tcpConn, ok := client.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	// Client -> Stream
	wg.Add(1)
	go func() {
		defer wg.Done()
		lw := &bandwidthLimitedWriter{w: stream, remaining: remaining, onLimit: closeAll}
		n, err := io.Copy(lw, client)
		totalBytes.Add(n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errBandwidthLimitExceeded) {
			log.Printf("Error copying client->stream: %v", err)
		}
		if errors.Is(err, errBandwidthLimitExceeded) {
			closeAll()
		}
		// Signal EOF to stream
		if tcpConn, ok := stream.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	return totalBytes.Load()
}

type bandwidthChargingWriter struct {
	w       io.Writer
	consume func(bytes int64) (bool, error)
	onLimit func()
}

func (cw *bandwidthChargingWriter) Write(p []byte) (int, error) {
	if cw.consume == nil {
		return cw.w.Write(p)
	}
	const maxChunk = 32 * 1024
	writtenTotal := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxChunk {
			chunk = p[:maxChunk]
		}
		allowed, err := cw.consume(int64(len(chunk)))
		if err != nil {
			return writtenTotal, err
		}
		if !allowed {
			if cw.onLimit != nil {
				cw.onLimit()
			}
			if writtenTotal > 0 {
				return writtenTotal, errBandwidthLimitExceeded
			}
			return 0, errBandwidthLimitExceeded
		}
		n, err := cw.w.Write(chunk)
		if n > 0 {
			writtenTotal += n
			p = p[n:]
		}
		if err != nil {
			return writtenTotal, err
		}
		if n < len(chunk) {
			return writtenTotal, io.ErrShortWrite
		}
	}
	return writtenTotal, nil
}

func copyBidirectionalWithReaderCharging(client net.Conn, stream net.Conn, streamReader *bufio.Reader, consume func(bytes int64) (bool, error)) int64 {
	var wg sync.WaitGroup
	var totalBytes atomic.Int64
	var closeOnce sync.Once

	closeAll := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = stream.Close()
		})
	}

	// Stream (via reader) -> Client
	wg.Add(1)
	go func() {
		defer wg.Done()
		cw := &bandwidthChargingWriter{w: client, consume: consume, onLimit: closeAll}
		n, err := io.Copy(cw, streamReader)
		totalBytes.Add(n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errBandwidthLimitExceeded) {
			log.Printf("Error copying stream->client: %v", err)
		}
		if errors.Is(err, errBandwidthLimitExceeded) {
			closeAll()
		}
		if tcpConn, ok := client.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	// Client -> Stream
	wg.Add(1)
	go func() {
		defer wg.Done()
		cw := &bandwidthChargingWriter{w: stream, consume: consume, onLimit: closeAll}
		n, err := io.Copy(cw, client)
		totalBytes.Add(n)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errBandwidthLimitExceeded) {
			log.Printf("Error copying client->stream: %v", err)
		}
		if errors.Is(err, errBandwidthLimitExceeded) {
			closeAll()
		}
		if tcpConn, ok := stream.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	return totalBytes.Load()
}
