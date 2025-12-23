package ingress

import (
	"bufio"
	"gopublic/internal/dashboard"
	"gopublic/internal/server"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

type Ingress struct {
	Registry    *server.TunnelRegistry
	DashHandler *dashboard.Handler
	Port        string
}

func NewIngress(port string, registry *server.TunnelRegistry, dash *dashboard.Handler) *Ingress {
	return &Ingress{
		Registry:    registry,
		DashHandler: dash,
		Port:        port,
	}
}

func (i *Ingress) Handler() http.Handler {
	// Set Gin to release mode
	gin.SetMode(gin.ReleaseMode)

	r := gin.Default()

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

func (i *Ingress) handleRequest(c *gin.Context) {
	host := c.Request.Host
	if strings.Contains(host, ":") {
		host = strings.Split(host, ":")[0]
	}

	rootDomain := os.Getenv("DOMAIN_NAME")

	// DEBUG LOG
	log.Printf("Ingress Debug: Host=%s, RootDomain=%s", host, rootDomain)

	// 1. Determine Routes
	isLocalDev := rootDomain == "127.0.0.1" || rootDomain == "localhost"
	isDashboard := (rootDomain != "" && host == "app."+rootDomain) || (isLocalDev && host == rootDomain)

	// 2. Landing Page (Only if NOT local dev and matches root)
	if !isLocalDev && rootDomain != "" && host == rootDomain {
		c.Header("Content-Type", "text/html")
		c.String(http.StatusOK, "<h1>Welcome to GoPublic</h1><p>Fast, simple, secure tunnels.</p><a href='http://app."+rootDomain+"'>Go to Dashboard</a>")
		return
	}

	// 3. Dashboard
	if isDashboard {
		if c.Request.URL.Path == "/login" {
			i.DashHandler.Login(c)
			return
		}
		if c.Request.URL.Path == "/" {
			i.DashHandler.Index(c)
			return
		}
		if c.Request.URL.Path == "/auth/telegram" {
			i.DashHandler.TelegramCallback(c)
			return
		}
		if c.Request.URL.Path == "/logout" {
			i.DashHandler.Logout(c)
			return
		}
		c.String(http.StatusNotFound, "Not Found")
		return
	}

	// 3. Look up session (User Tunnels)
	session, ok := i.Registry.GetSession(host)
	if !ok {
		c.String(http.StatusNotFound, "Tunnel not found for host: %s", host)
		return
	}

	// 2. Open Stream
	stream, err := session.Open()
	if err != nil {
		log.Printf("Failed to open stream for host %s: %v", host, err)
		c.String(http.StatusBadGateway, "Failed to connect to tunnel client")
		return
	}
	defer stream.Close()

	// 3. Forward Request
	// We need to clone the request or just write it.
	// `c.Request` is the incoming request.
	// CAUTION: RequestURI might be missing or absolute URI depending on how it came in.
	// We want to send path and query.

	// We'll write the request as valid HTTP to the stream.
	// But we should verify if we need to modify headers (e.g. X-Forwarded-For).

	// Write entire request to session stream
	err = c.Request.Write(stream)
	if err != nil {
		log.Printf("Failed to write request to stream: %v", err)
		c.Status(http.StatusBadGateway)
		return
	}

	// 4. Read Response
	// We use http.ReadResponse to parse the bytes coming back from the tunnel
	resp, err := http.ReadResponse(bufio.NewReader(stream), c.Request)
	if err != nil {
		log.Printf("Failed to read response from stream: %v", err)
		c.Status(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 5. Write Response back to user
	for k, vv := range resp.Header {
		for _, v := range vv {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}
