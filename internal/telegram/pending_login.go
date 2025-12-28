package telegram

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"

	PendingLoginTTL = 5 * time.Minute
)

// PendingLogin represents a pending authentication request
type PendingLogin struct {
	Hash       string
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	Status     string
	TelegramID int64
	FirstName  string
	LastName   string
	Username   string
	PhotoURL   string
	IsLinking  bool
	UserID     uint
}

// PendingLoginStore is a thread-safe in-memory store for pending logins
type PendingLoginStore struct {
	mu     sync.RWMutex
	logins map[string]*PendingLogin
	stopCh chan struct{}
}

// NewPendingLoginStore creates a new store and starts cleanup goroutine
func NewPendingLoginStore() *PendingLoginStore {
	store := &PendingLoginStore{
		logins: make(map[string]*PendingLogin),
		stopCh: make(chan struct{}),
	}
	go store.cleanupLoop()
	return store
}

// generateHash creates a cryptographically random 32-byte hash
func generateHash() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// Create adds a new pending login and returns its hash
func (s *PendingLoginStore) Create(ip, userAgent string, isLinking bool, userID uint) (string, error) {
	hash, err := generateHash()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.logins[hash] = &PendingLogin{
		Hash:      hash,
		IP:        ip,
		UserAgent: userAgent,
		CreatedAt: time.Now(),
		Status:    StatusPending,
		IsLinking: isLinking,
		UserID:    userID,
	}

	return hash, nil
}

// Get retrieves a pending login by hash (returns nil if expired)
func (s *PendingLoginStore) Get(hash string) (*PendingLogin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	login, ok := s.logins[hash]
	if !ok {
		return nil, false
	}

	if time.Since(login.CreatedAt) > PendingLoginTTL {
		return nil, false
	}

	return login, true
}

// Approve marks login as approved and stores Telegram user info
func (s *PendingLoginStore) Approve(hash string, telegramID int64, firstName, lastName, username, photoURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	login, ok := s.logins[hash]
	if !ok {
		return false
	}

	if time.Since(login.CreatedAt) > PendingLoginTTL {
		delete(s.logins, hash)
		return false
	}

	if login.Status != StatusPending {
		return false
	}

	login.Status = StatusApproved
	login.TelegramID = telegramID
	login.FirstName = firstName
	login.LastName = lastName
	login.Username = username
	login.PhotoURL = photoURL

	return true
}

// Reject marks login as rejected
func (s *PendingLoginStore) Reject(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	login, ok := s.logins[hash]
	if !ok {
		return false
	}

	if login.Status != StatusPending {
		return false
	}

	login.Status = StatusRejected
	return true
}

// Consume removes and returns approved login (one-time use)
func (s *PendingLoginStore) Consume(hash string) (*PendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	login, ok := s.logins[hash]
	if !ok {
		return nil, false
	}

	delete(s.logins, hash)

	if login.Status != StatusApproved {
		return nil, false
	}

	return login, true
}

// Delete removes a pending login by hash
func (s *PendingLoginStore) Delete(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.logins, hash)
}

// cleanupLoop periodically removes expired entries
func (s *PendingLoginStore) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

func (s *PendingLoginStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for hash, login := range s.logins {
		if now.Sub(login.CreatedAt) > PendingLoginTTL {
			delete(s.logins, hash)
		}
	}
}

// Stop stops the cleanup goroutine
func (s *PendingLoginStore) Stop() {
	close(s.stopCh)
}
