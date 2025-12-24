package server

import (
	"sync"

	"github.com/hashicorp/yamux"
)

// UserSession represents an active user connection.
type UserSession struct {
	UserID  uint
	Session *yamux.Session
	Domains []string
}

// UserSessionRegistry tracks active sessions per user.
// Only one active session per user is allowed.
type UserSessionRegistry struct {
	mu       sync.RWMutex
	sessions map[uint]*UserSession // userID -> session
}

// NewUserSessionRegistry creates a new registry.
func NewUserSessionRegistry() *UserSessionRegistry {
	return &UserSessionRegistry{
		sessions: make(map[uint]*UserSession),
	}
}

// GetSession returns the active session for a user, if any.
func (r *UserSessionRegistry) GetSession(userID uint) (*UserSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sess, ok := r.sessions[userID]
	return sess, ok
}

// IsConnected checks if a user has an active session.
func (r *UserSessionRegistry) IsConnected(userID uint) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.sessions[userID]
	return ok
}

// Register registers a new session for a user.
// Returns the old session if one existed (caller should close it).
func (r *UserSessionRegistry) Register(userID uint, session *yamux.Session, domains []string) *UserSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.sessions[userID]
	r.sessions[userID] = &UserSession{
		UserID:  userID,
		Session: session,
		Domains: domains,
	}
	return old
}

// Unregister removes a user's session.
func (r *UserSessionRegistry) Unregister(userID uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, userID)
}
