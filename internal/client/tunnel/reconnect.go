package tunnel

import (
	"context"
	"fmt"
	"time"

	"gopublic/internal/client/logger"
)

// ReconnectConfig holds reconnection parameters
type ReconnectConfig struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
	MaxAttempts  int // 0 = infinite
}

// DefaultReconnectConfig returns sensible defaults for reconnection
func DefaultReconnectConfig() *ReconnectConfig {
	return &ReconnectConfig{
		InitialDelay: 1 * time.Second,
		MaxDelay:     60 * time.Second,
		Multiplier:   2.0,
		MaxAttempts:  0, // Infinite
	}
}

// StartWithReconnect starts the tunnel with automatic reconnection on failure
func (t *Tunnel) StartWithReconnect(ctx context.Context, cfg *ReconnectConfig) error {
	if cfg == nil {
		cfg = DefaultReconnectConfig()
	}

	// Monitor context cancellation and shutdown tunnel when cancelled
	go func() {
		<-ctx.Done()
		logger.Info("Context cancelled, shutting down tunnel...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.Shutdown(shutdownCtx)
	}()

	attempt := 0
	delay := cfg.InitialDelay

	for {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			logger.Info("Tunnel shutdown requested")
			return ctx.Err()
		default:
		}

		attempt++

		// Check max attempts
		if cfg.MaxAttempts > 0 && attempt > cfg.MaxAttempts {
			t.publishStatus("error", fmt.Sprintf("Max reconnection attempts (%d) exceeded", cfg.MaxAttempts))
			return fmt.Errorf("max reconnection attempts (%d) exceeded", cfg.MaxAttempts)
		}

		// Wait before reconnecting (except first attempt)
		if attempt > 1 {
			logger.Info("Reconnecting in %v (attempt %d)...", delay, attempt)
			t.publishStatus("reconnecting", fmt.Sprintf("Reconnecting in %v (attempt %d)...", delay, attempt))

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				logger.Info("Tunnel shutdown requested during reconnect wait")
				return ctx.Err()
			}
		}

		// Try to connect
		logger.Info("Connecting to %s...", t.ServerAddr)
		err := t.Start()

		if err != nil {
			// Don't retry on "already connected" error - this is not transient
			if IsAlreadyConnectedError(err) {
				logger.Error("Session conflict: %v", err)
				t.publishStatus("error", fmt.Sprintf("Session conflict: %v", err))
				return err
			}

			logger.Warn("Connection failed: %v", err)
			t.publishStatus("connection_failed", fmt.Sprintf("Connection failed: %v (retry in %v)", err, delay))

			// Exponential backoff
			delay = time.Duration(float64(delay) * cfg.Multiplier)
			if delay > cfg.MaxDelay {
				delay = cfg.MaxDelay
			}
			continue
		}

		// Connection was successful but ended (session closed)
		// This happens when handleSession returns normally (e.g., server closed connection)
		logger.Info("Connection ended, will reconnect...")
		t.publishStatus("disconnected", "Connection ended, reconnecting...")

		// Reset backoff on successful connection
		attempt = 0
		delay = cfg.InitialDelay
	}
}
