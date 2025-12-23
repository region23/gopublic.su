package ingress

import (
	"bufio"
	"gopublic/internal/server"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

type Ingress struct {
	Registry *server.TunnelRegistry
	Port     string
}

func NewIngress(port string, registry *server.TunnelRegistry) *Ingress {
	return &Ingress{
		Registry: registry,
		Port:     port,
	}
}

func (i *Ingress) Handler() http.Handler {
	// Set Gin to release mode
	gin.SetMode(gin.ReleaseMode)

	r := gin.Default()

	// Catch-all handler
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

	// 1. Landing Page
	if rootDomain != "" && host == rootDomain {
		c.Header("Content-Type", "text/html")
		c.String(http.StatusOK, "<h1>Welcome to GoPublic</h1><p>Fast, simple, secure tunnels.</p><a href='http://app."+rootDomain+"'>Go to Dashboard</a>")
		return
	}

	// 2. Dashboard
	if rootDomain != "" && host == "app."+rootDomain {
		c.Header("Content-Type", "text/html")
		c.String(http.StatusOK, "<h1>GoPublic Dashboard</h1><p>Login with Google (Coming Soon)</p>")
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
