package tunnel

import (
	"context"
	"fmt"
	"sync"

	"gopublic/internal/client/events"
	"gopublic/internal/client/logger"
	"gopublic/internal/client/stats"
)

// TunnelManager coordinates multiple tunnel connections using a shared session.
type TunnelManager struct {
	ServerAddr string
	Token      string
	Force      bool // Force disconnect existing sessions
	NoCache    bool // Add Cache-Control: no-store to responses
	tunnels    []*ManagedTunnel
	mu         sync.Mutex
	eventBus   *events.Bus
	stats      *stats.Stats

	// Shared tunnel instance (used when starting)
	sharedTunnel *SharedTunnel
	cancelFunc   context.CancelFunc
}

// ManagedTunnel wraps a tunnel with its metadata
type ManagedTunnel struct {
	Name      string
	LocalPort string
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

// SetEventBus sets the event bus for all tunnels
func (tm *TunnelManager) SetEventBus(eventBus *events.Bus) {
	tm.eventBus = eventBus
}

// SetStats sets the stats tracker for all tunnels
func (tm *TunnelManager) SetStats(stats *stats.Stats) {
	tm.stats = stats
}

// SetNoCache enables Cache-Control: no-store header on all responses
func (tm *TunnelManager) SetNoCache(noCache bool) {
	tm.NoCache = noCache
}

// AddTunnel adds a tunnel configuration to the manager
func (tm *TunnelManager) AddTunnel(name, localPort, subdomain string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	mt := &ManagedTunnel{
		Name:      name,
		LocalPort: localPort,
		Subdomain: subdomain,
	}
	tm.tunnels = append(tm.tunnels, mt)
}

// StartAll starts all configured tunnels using a single shared connection.
func (tm *TunnelManager) StartAll(ctx context.Context) error {
	tm.mu.Lock()
	if len(tm.tunnels) == 0 {
		tm.mu.Unlock()
		return fmt.Errorf("no tunnels configured")
	}

	// Build subdomain -> localPort mapping
	tunnelMap := make(map[string]string)
	for _, mt := range tm.tunnels {
		tunnelMap[mt.Subdomain] = mt.LocalPort
		logger.Info("Configured tunnel '%s': %s -> %s", mt.Name, resolveLocalAddr(mt.LocalPort), mt.Subdomain)
	}

	// Create shared tunnel
	st := NewSharedTunnel(tm.ServerAddr, tm.Token, tunnelMap)
	st.SetEventBus(tm.eventBus)
	st.SetStats(tm.stats)
	st.SetForce(tm.Force)
	st.SetNoCache(tm.NoCache)

	tm.sharedTunnel = st

	// Create cancellable context
	tunnelCtx, cancel := context.WithCancel(ctx)
	tm.cancelFunc = cancel
	tm.mu.Unlock()

	// Start shared tunnel with reconnection
	return st.StartWithReconnect(tunnelCtx, nil)
}

// StopAll stops all running tunnels
func (tm *TunnelManager) StopAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.cancelFunc != nil {
		tm.cancelFunc()
		tm.cancelFunc = nil
	}

	if tm.sharedTunnel != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*1e9) // 5 seconds
		defer cancel()
		tm.sharedTunnel.Shutdown(ctx)
		tm.sharedTunnel = nil
	}
}
