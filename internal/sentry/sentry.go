package sentry

import (
	"fmt"
	"log"

	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
)

// CaptureError logs an error locally and reports it to Sentry.
// Use this for errors outside of HTTP request context (startup, background tasks).
func CaptureError(err error, message string) {
	log.Printf("%s: %v", message, err)
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetExtra("message", message)
		sentry.CaptureException(err)
	})
}

// CaptureErrorWithContext logs an error and reports it to Sentry with HTTP request context.
// This preserves request data (URL, headers, user info) in Sentry events.
func CaptureErrorWithContext(c *gin.Context, err error, message string) {
	log.Printf("%s: %v", message, err)
	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.WithScope(func(scope *sentry.Scope) {
			scope.SetExtra("message", message)
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
