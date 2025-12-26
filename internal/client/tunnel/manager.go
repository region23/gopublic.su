package tunnel

import (
	"context"
	"fmt"
	"sync"

	"gopublic/internal/client/logger"
)

// TunnelManager coordinates multiple tunnel connections
type TunnelManager struct {
	ServerAddr string
	Token      string
	Force      bool // Force disconnect existing sessions
	tunnels    []*ManagedTunnel
	mu         sync.Mutex
}

// ManagedTunnel wraps a tunnel with its metadata
type ManagedTunnel struct {
	Name      string
	Tunnel    *Tunnel
	Cancel    context.CancelFunc
	Subdomain string
}

// NewTunnelManager creates a new tunnel manager
func NewTunnelManager(serverAddr, token string) *TunnelManager {
	return &TunnelManager{
		ServerAddr: serverAddr,
		Token:      token,
		tunnels:    make([]*ManagedTunnel, 0),
	}
}

// SetForce sets the force flag to disconnect existing sessions
func (tm *TunnelManager) SetForce(force bool) {
	tm.Force = force
}

// AddTunnel adds a tunnel configuration to the manager
func (tm *TunnelManager) AddTunnel(name, localPort, subdomain string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	t := NewTunnel(tm.ServerAddr, tm.Token, localPort)
	t.Subdomain = subdomain
	t.Force = tm.Force

	mt := &ManagedTunnel{
		Name:      name,
		Tunnel:    t,
		Subdomain: subdomain,
	}
	tm.tunnels = append(tm.tunnels, mt)
}

// StartAll starts all configured tunnels concurrently
func (tm *TunnelManager) StartAll(ctx context.Context) error {
	if len(tm.tunnels) == 0 {
		return fmt.Errorf("no tunnels configured")
	}

	var wg sync.WaitGroup
	errChan := make(chan error, len(tm.tunnels))

	for _, mt := range tm.tunnels {
		wg.Add(1)
		tunnelCtx, cancel := context.WithCancel(ctx)
		mt.Cancel = cancel

		go func(mt *ManagedTunnel, ctx context.Context) {
			defer wg.Done()
			logger.Info("Starting tunnel '%s': localhost:%s -> %s", mt.Name, mt.Tunnel.LocalPort, mt.Subdomain)

			err := mt.Tunnel.StartWithReconnect(ctx, nil)
			if err != nil && err != context.Canceled {
				errChan <- fmt.Errorf("tunnel '%s' failed: %v", mt.Name, err)
			}
		}(mt, tunnelCtx)
	}

	// Wait for context cancellation or first error
	select {
	case err := <-errChan:
		tm.StopAll()
		return err
	case <-ctx.Done():
		tm.StopAll()
		wg.Wait()
		return ctx.Err()
	}
}

// StopAll stops all running tunnels
func (tm *TunnelManager) StopAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	for _, mt := range tm.tunnels {
		if mt.Cancel != nil {
			mt.Cancel()
		}
	}
}
