package ingress

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"gopublic/internal/config"
	"gopublic/internal/dashboard"
	"gopublic/internal/middleware"
	"gopublic/internal/server"
)

// hostPattern validates hostnames (RFC 1123 compliant + localhost).
// Allows alphanumeric, hyphens, dots; max 253 chars; labels max 63 chars.
var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?)*$`)

type Ingress struct {
	Registry    *server.TunnelRegistry
	DashHandler *dashboard.Handler
	Port        string
	RootDomain  string // Root domain for routing
	ProjectName string // Project name for branding
	IsSecure    bool   // Whether running in secure mode
	GitHubRepo  string // GitHub repo for client downloads (e.g., "username/gopublic")
}

// NewIngressWithConfig creates a new ingress with the given configuration.
func NewIngressWithConfig(cfg *config.Config, registry *server.TunnelRegistry, dash *dashboard.Handler) *Ingress {
	return &Ingress{
		Registry:    registry,
		DashHandler: dash,
		Port:        cfg.IngressPort(),
		RootDomain:  cfg.Domain,
		ProjectName: cfg.ProjectName,
		IsSecure:    cfg.IsSecure(),
		GitHubRepo:  cfg.GitHubRepo,
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
	}
}

func (i *Ingress) Handler() http.Handler {
	// Set Gin to release mode
	gin.SetMode(gin.ReleaseMode)

	r := gin.Default()

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
	default:
		scheme := "http"
		if i.IsSecure {
			scheme = "https"
		}
		c.HTML(http.StatusOK, "landing.html", gin.H{
			"ProjectName":  i.ProjectName,
			"RootDomain":   i.RootDomain,
			"DashboardURL": scheme + "://app." + i.RootDomain,
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
INSTALL_DIR="/usr/local/bin"

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

# Use temp directory to avoid permission issues
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

echo "Downloading ${BINARY}..."
curl -fsSL "$URL" -o "$TMPDIR/$BINARY_NAME"
chmod +x "$TMPDIR/$BINARY_NAME"

# Try to install to /usr/local/bin
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMPDIR/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"
  echo ""
  echo "Installed: $INSTALL_DIR/$BINARY_NAME"
  echo "Run 'gopublic --help' to get started."
else
  # Copy to current directory if no write access to install dir
  cp "$TMPDIR/$BINARY_NAME" "./$BINARY_NAME"
  echo ""
  echo "Downloaded: ./${BINARY_NAME}"
  echo ""
  echo "To install globally, run:"
  echo "  sudo mv ${BINARY_NAME} ${INSTALL_DIR}/"
fi
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
	case "/api/regenerate-token":
		if c.Request.Method == http.MethodPost {
			i.DashHandler.RegenerateToken(c)
		} else {
			c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	default:
		c.String(http.StatusNotFound, "Not Found")
	}
}

// proxyToTunnel forwards the request to a tunnel client.
func (i *Ingress) proxyToTunnel(c *gin.Context, host string) {
	// Look up session
	session, ok := i.Registry.GetSession(host)
	if !ok {
		c.String(http.StatusNotFound, "Tunnel not found for host: %s", host)
		return
	}

	// Open stream to tunnel client
	stream, err := session.Open()
	if err != nil {
		log.Printf("Failed to open stream for host %s: %v", host, err)
		c.String(http.StatusBadGateway, "Failed to connect to tunnel client")
		return
	}
	defer stream.Close()

	// Forward request
	if err := c.Request.Write(stream); err != nil {
		log.Printf("Failed to write request to stream: %v", err)
		c.Status(http.StatusBadGateway)
		return
	}

	// Read and forward response
	resp, err := http.ReadResponse(bufio.NewReader(stream), c.Request)
	if err != nil {
		log.Printf("Failed to read response from stream: %v", err)
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

	// Write status and body
	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}
