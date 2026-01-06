package server

import (
	"sync"

	"github.com/hashicorp/yamux"
)

// TunnelEntry contains session and user info for a registered tunnel
type TunnelEntry struct {
	Session *yamux.Session
	UserID  uint
	// BandwidthExempt disables bandwidth limits for this tunnel's user.
	BandwidthExempt bool
}

// TunnelRegistry manages the mapping between hostnames and active Yamux sessions.
type TunnelRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*TunnelEntry
}

func NewTunnelRegistry() *TunnelRegistry {
	return &TunnelRegistry{
		sessions: make(map[string]*TunnelEntry),
	}
}

// Register maps a hostname to a session with user ID.
func (r *TunnelRegistry) Register(hostname string, session *yamux.Session, userID uint, bandwidthExempt bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[hostname] = &TunnelEntry{
		Session: session,
		UserID:  userID,
		BandwidthExempt: bandwidthExempt,
	}
}

// Unregister removes a mapping.
func (r *TunnelRegistry) Unregister(hostname string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, hostname)
}

// GetSession returns the session for a given hostname (for backward compatibility).
func (r *TunnelRegistry) GetSession(hostname string) (*yamux.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.sessions[hostname]
	if !ok {
		return nil, false
	}
	return entry.Session, true
}

// GetEntry returns the full tunnel entry for a given hostname.
func (r *TunnelRegistry) GetEntry(hostname string) (*TunnelEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.sessions[hostname]
	return entry, ok
}
