package events

import (
	"sync"
	"time"
)

// EventType represents the type of event.
type EventType int

const (
	// Connection lifecycle events
	EventConnecting EventType = iota
	EventConnected
	EventDisconnected
	EventReconnecting

	// Detailed connection status events
	EventConnectionStatus // For showing detailed connection progress

	// Request/Response events
	EventRequestStart
	EventRequestComplete

	// Error events
	EventError

	// Log events (for TUI display)
	EventLog

	// Tunnel info events
	EventTunnelReady
)

// String returns a human-readable name for the event type.
func (t EventType) String() string {
	switch t {
	case EventConnecting:
		return "connecting"
	case EventConnected:
		return "connected"
	case EventDisconnected:
		return "disconnected"
	case EventReconnecting:
		return "reconnecting"
	case EventConnectionStatus:
		return "connection_status"
	case EventRequestStart:
		return "request_start"
	case EventRequestComplete:
		return "request_complete"
	case EventError:
		return "error"
	case EventLog:
		return "log"
	case EventTunnelReady:
		return "tunnel_ready"
	default:
		return "unknown"
	}
}

// Event represents an event in the system.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Data      interface{}
}

// ConnectedData contains data for EventConnected.
type ConnectedData struct {
	ServerAddr       string
	BoundDomains     []string
	Latency          time.Duration
	BandwidthToday   int64 // Bytes used today
	BandwidthTotal   int64 // Total bytes used all time
	BandwidthLimit   int64 // Daily bandwidth limit in bytes
}

// ReconnectingData contains data for EventReconnecting.
type ReconnectingData struct {
	Attempt int
	Delay   time.Duration
	Error   error
}

// RequestData contains data for request events.
type RequestData struct {
	Method   string
	Path     string
	Status   int
	Duration time.Duration
	Bytes    int64
}

// ErrorData contains data for EventError.
type ErrorData struct {
	Error   error
	Context string
}

// TunnelReadyData contains data for EventTunnelReady.
type TunnelReadyData struct {
	Name         string
	LocalPort    string
	BoundDomains []string
	Scheme       string
}

// LogData contains data for EventLog.
type LogData struct {
	Level   string // "info", "warn", "error"
	Message string
}

// ConnectionStatusData contains data for EventConnectionStatus.
type ConnectionStatusData struct {
	Stage   string // "dialing", "tls_handshake", "yamux_init", "authenticating", "requesting_tunnel"
	Message string // Human-readable message
}

// Bus is a simple pub/sub event bus with fan-out delivery.
type Bus struct {
	mu          sync.RWMutex
	subscribers []chan Event
	bufferSize  int
	closed      bool
}

// NewBus creates a new event bus.
func NewBus() *Bus {
	return &Bus{
		subscribers: make([]chan Event, 0),
		bufferSize:  100, // Default buffer size per subscriber
	}
}

// NewBusWithBuffer creates a new event bus with custom buffer size.
func NewBusWithBuffer(bufferSize int) *Bus {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &Bus{
		subscribers: make([]chan Event, 0),
		bufferSize:  bufferSize,
	}
}

// Subscribe returns a channel that receives all published events.
// The caller is responsible for consuming events to avoid blocking.
func (b *Bus) Subscribe() <-chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		// Return a closed channel if bus is closed
		ch := make(chan Event)
		close(ch)
		return ch
	}

	ch := make(chan Event, b.bufferSize)
	b.subscribers = append(b.subscribers, ch)
	return ch
}

// Unsubscribe removes a subscriber channel.
func (b *Bus) Unsubscribe(ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, sub := range b.subscribers {
		if sub == ch {
			close(sub)
			b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
			return
		}
	}
}

// Publish sends an event to all subscribers.
// Non-blocking: if a subscriber's buffer is full, the event is dropped for that subscriber.
func (b *Bus) Publish(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Subscriber buffer full, drop event
		}
	}
}

// PublishType is a convenience method to publish an event with just a type.
func (b *Bus) PublishType(eventType EventType) {
	b.Publish(Event{Type: eventType})
}

// PublishError publishes an error event.
func (b *Bus) PublishError(err error, context string) {
	b.Publish(Event{
		Type: EventError,
		Data: ErrorData{Error: err, Context: context},
	})
}

// PublishLog publishes a log event.
func (b *Bus) PublishLog(level, message string) {
	b.Publish(Event{
		Type: EventLog,
		Data: LogData{Level: level, Message: message},
	})
}

// Close closes the event bus and all subscriber channels.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	b.closed = true
	for _, ch := range b.subscribers {
		close(ch)
	}
	b.subscribers = nil
}

// SubscriberCount returns the number of active subscribers.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}
