package logger

import (
	"fmt"
	"io"
	"log"
	"sync"

	"gopublic/internal/client/events"
)

// Logger wraps standard logging with event bus integration for TUI mode.
type Logger struct {
	mu       sync.RWMutex
	eventBus *events.Bus
	tuiMode  bool
}

var (
	defaultLogger = &Logger{}
	originalWriter io.Writer
)

// SetEventBus sets the event bus for TUI mode logging.
func SetEventBus(bus *events.Bus) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.eventBus = bus
}

// SetTUIMode enables or disables TUI mode.
// In TUI mode, logs are sent to event bus instead of stderr.
func SetTUIMode(enabled bool) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.tuiMode = enabled

	if enabled {
		// Capture original writer and discard standard log output
		originalWriter = log.Writer()
		log.SetOutput(io.Discard)
	} else {
		// Restore original log output
		if originalWriter != nil {
			log.SetOutput(originalWriter)
		}
	}
}

// Info logs an informational message.
func Info(format string, args ...interface{}) {
	defaultLogger.log("info", format, args...)
}

// Warn logs a warning message.
func Warn(format string, args ...interface{}) {
	defaultLogger.log("warn", format, args...)
}

// Error logs an error message.
func Error(format string, args ...interface{}) {
	defaultLogger.log("error", format, args...)
}

func (l *Logger) log(level, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)

	l.mu.RLock()
	tuiMode := l.tuiMode
	bus := l.eventBus
	l.mu.RUnlock()

	if tuiMode && bus != nil {
		bus.PublishLog(level, message)
	} else {
		// Fallback to standard log
		log.Print(message)
	}
}
