package metrics

import (
	"time"

	"github.com/gin-gonic/gin"
)

// AppMetrics holds application-specific metrics.
type AppMetrics struct {
	// Tunnel metrics
	ActiveTunnels     *Gauge
	TunnelConnections *Counter
	TunnelErrors      *Counter

	// User metrics
	UsersTotal *Gauge

	// HTTP metrics
	RequestsTotal   *Counter
	RequestDuration *Histogram
	ResponseCodes   map[int]*Counter

	// Internal
	m *Metrics
}

// NewAppMetrics creates and registers all application metrics.
func NewAppMetrics() *AppMetrics {
	m := New()

	am := &AppMetrics{
		m: m,

		ActiveTunnels: m.NewGauge(
			"gopublic_active_tunnels",
			"Number of currently active tunnel connections",
			nil,
		),

		TunnelConnections: m.NewCounter(
			"gopublic_tunnel_connections_total",
			"Total number of tunnel connections established",
			nil,
		),

		TunnelErrors: m.NewCounter(
			"gopublic_tunnel_errors_total",
			"Total number of tunnel connection errors",
			nil,
		),

		UsersTotal: m.NewGauge(
			"gopublic_users_total",
			"Total number of registered users",
			nil,
		),

		RequestsTotal: m.NewCounter(
			"gopublic_http_requests_total",
			"Total number of HTTP requests",
			nil,
		),

		RequestDuration: m.NewHistogram(
			"gopublic_http_request_duration_seconds",
			"HTTP request duration in seconds",
			DefaultBuckets,
			nil,
		),

		ResponseCodes: make(map[int]*Counter),
	}

	// Pre-create common response code counters
	for _, code := range []int{200, 201, 204, 400, 401, 403, 404, 500, 502, 503} {
		am.ResponseCodes[code] = m.NewCounter(
			"gopublic_http_responses_total",
			"Total number of HTTP responses by status code",
			map[string]string{"code": statusCodeToString(code)},
		)
	}

	return am
}

// Handler returns the metrics endpoint handler.
func (am *AppMetrics) Handler() gin.HandlerFunc {
	return am.m.Handler()
}

// Middleware returns a Gin middleware that records request metrics.
func (am *AppMetrics) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		// Record duration
		am.RequestDuration.ObserveDuration(start)

		// Increment request counter
		am.RequestsTotal.Inc()

		// Increment response code counter
		code := c.Writer.Status()
		if counter, ok := am.ResponseCodes[code]; ok {
			counter.Inc()
		}
	}
}

// TunnelConnected should be called when a tunnel connects.
func (am *AppMetrics) TunnelConnected() {
	am.ActiveTunnels.Inc()
	am.TunnelConnections.Inc()
}

// TunnelDisconnected should be called when a tunnel disconnects.
func (am *AppMetrics) TunnelDisconnected() {
	am.ActiveTunnels.Dec()
}

// TunnelError should be called when a tunnel error occurs.
func (am *AppMetrics) TunnelError() {
	am.TunnelErrors.Inc()
}

// SetUsersTotal sets the total users gauge to an absolute value.
func (am *AppMetrics) SetUsersTotal(n float64) {
	am.UsersTotal.Set(n)
}

// UserCreated should be called when a new user is registered.
func (am *AppMetrics) UserCreated() {
	am.UsersTotal.Inc()
}

func statusCodeToString(code int) string {
	switch code {
	case 200:
		return "200"
	case 201:
		return "201"
	case 204:
		return "204"
	case 400:
		return "400"
	case 401:
		return "401"
	case 403:
		return "403"
	case 404:
		return "404"
	case 500:
		return "500"
	case 502:
		return "502"
	case 503:
		return "503"
	default:
		return "other"
	}
}
