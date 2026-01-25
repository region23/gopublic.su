package sentry

import (
	"fmt"
	"log"
	"strings"

	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
)

// ignoredErrors contains error messages that should be logged but not sent to Sentry.
// These are typically caused by bots/scanners or normal client disconnects and create noise.
var ignoredErrors = []string{
	"acme/autocert: missing server name",              // TLS connections without SNI (bots scanning port 4443)
	"first record does not look like a TLS handshake", // Plain TCP connections to TLS port (bots/scanners)
	"tls: unsupported SSLv2 handshake received",       // Ancient/invalid handshake (usually scanners)
	"host not configured",                             // TLS SNI is not covered by autocert HostPolicy
	"connection reset by peer",                        // Client disconnected abruptly (sleep mode, network loss)
	"EOF",                                             // Client closed connection without graceful shutdown
	"broken pipe",                                     // Write to closed connection (client already gone)
	"use of closed network connection",                // Operation on already closed connection
}

// shouldIgnore checks if an error should be filtered out from Sentry.
func shouldIgnore(err error) bool {
	if err == nil {
		return true
	}

	// Treat socket timeouts as noise: scanners often connect and never speak.
	type timeoutError interface{ Timeout() bool }
	if te, ok := err.(timeoutError); ok && te.Timeout() {
		return true
	}

	errStr := err.Error()
	for _, ignored := range ignoredErrors {
		if strings.Contains(errStr, ignored) {
			return true
		}
	}
	return false
}

// CaptureError logs an error locally and reports it to Sentry.
// Use this for errors outside of HTTP request context (startup, background tasks).
func CaptureError(err error, message string) {
	log.Printf("%s: %v", message, err)
	if shouldIgnore(err) {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetExtra("message", message)
		sentry.CaptureException(err)
	})
}

// CaptureErrorWithContext logs an error and reports it to Sentry with HTTP request context.
// This preserves request data (URL, headers, user info) in Sentry events.
func CaptureErrorWithContext(c *gin.Context, err error, message string) {
	log.Printf("%s: %v", message, err)
	if shouldIgnore(err) {
		return
	}
	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.WithScope(func(scope *sentry.Scope) {
			scope.SetExtra("message", message)
			// Helpful request diagnostics without dumping sensitive headers.
			if c != nil && c.Request != nil {
				scope.SetTag("http.method", c.Request.Method)
				scope.SetTag("http.host", c.Request.Host)
				scope.SetTag("http.path", c.Request.URL.Path)
				scope.SetExtra("http.query", c.Request.URL.RawQuery)
				scope.SetExtra("http.remote_ip", c.ClientIP())
				scope.SetExtra("http.user_agent", c.Request.UserAgent())
				scope.SetExtra("http.referer", c.Request.Referer())
				scope.SetExtra("http.upgrade", c.Request.Header.Get("Upgrade"))
				scope.SetExtra("http.connection", c.Request.Header.Get("Connection"))
				if rid := c.Request.Header.Get("X-Request-Id"); rid != "" {
					scope.SetTag("request_id", rid)
				}
			}
			hub.CaptureException(err)
		})
	} else {
		// Fallback to global capture if no hub in context
		CaptureError(err, message)
	}
}

// CaptureErrorf logs and reports an error with a formatted message.
func CaptureErrorf(err error, format string, args ...interface{}) {
	CaptureError(err, fmt.Sprintf(format, args...))
}

// CaptureErrorWithContextf logs and reports an error with a formatted message and HTTP context.
func CaptureErrorWithContextf(c *gin.Context, err error, format string, args ...interface{}) {
	CaptureErrorWithContext(c, err, fmt.Sprintf(format, args...))
}
