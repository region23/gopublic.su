package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/securecookie"
)

// Errors for session management
var (
	ErrMissingSessionKey = errors.New("session keys not configured")
	ErrInvalidSessionKey = errors.New("invalid session key format")
)

// SessionConfig holds session manager configuration
type SessionConfig struct {
	// IsSecure sets the Secure flag on cookies (true for HTTPS)
	IsSecure bool
	// AllowInsecureKeys allows random key generation in dev mode
	// If false and keys are missing, NewSessionManager returns an error
	AllowInsecureKeys bool
}

// SessionManager handles secure cookie encoding/decoding
type SessionManager struct {
	sc       *securecookie.SecureCookie
	isSecure bool // Whether to set Secure flag on cookies
}

// SessionData represents the data stored in session cookie
type SessionData struct {
	UserID    uint  `json:"user_id"`
	CreatedAt int64 `json:"created_at"`
}

// Track whether we've already warned about missing keys (warn only once)
var (
	keyWarningOnce sync.Once
	keyWarningMsg  string
)

// NewSessionManager creates a new session manager.
// In production (AllowInsecureKeys=false), returns error if keys are not configured.
// In development (AllowInsecureKeys=true), generates random keys with a warning.
func NewSessionManager(cfg SessionConfig) (*SessionManager, error) {
	hashKey, err := getKey("SESSION_HASH_KEY", 32, cfg.AllowInsecureKeys)
	if err != nil {
		return nil, err
	}

	blockKey, err := getKey("SESSION_BLOCK_KEY", 32, cfg.AllowInsecureKeys)
	if err != nil {
		return nil, err
	}

	// Log warning once if using random keys
	keyWarningOnce.Do(func() {
		if keyWarningMsg != "" {
			log.Println(keyWarningMsg)
		}
	})

	sc := securecookie.New(hashKey, blockKey)
	sc.MaxAge(30 * 24 * 60 * 60) // 30 days

	return &SessionManager{
		sc:       sc,
		isSecure: cfg.IsSecure,
	}, nil
}

// getKey reads key from environment or generates a random one if allowed
func getKey(envVar string, length int, allowRandom bool) ([]byte, error) {
	keyHex := os.Getenv(envVar)
	if keyHex != "" {
		key, err := hex.DecodeString(keyHex)
		if err != nil {
			return nil, ErrInvalidSessionKey
		}
		if len(key) < length {
			return nil, ErrInvalidSessionKey
		}
		return key[:length], nil
	}

	// Key not set
	if !allowRandom {
		return nil, ErrMissingSessionKey
	}

	// Generate random key (sessions won't persist across restarts)
	key := make([]byte, length)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}

	// Store warning message to log once
	keyWarningMsg = "WARNING: Session keys not configured. Using random keys - sessions will not persist across server restarts. Set SESSION_HASH_KEY and SESSION_BLOCK_KEY environment variables for production."

	return key, nil
}

// SetSession creates a signed session cookie
func (sm *SessionManager) SetSession(w http.ResponseWriter, userID uint) error {
	data := SessionData{
		UserID:    userID,
		CreatedAt: time.Now().Unix(),
	}

	encoded, err := sm.sc.Encode("session", data)
	if err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    encoded,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60, // 30 days
		Secure:   sm.isSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	return nil
}

// GetSession reads and validates session cookie
func (sm *SessionManager) GetSession(r *http.Request) (*SessionData, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, err
	}

	var data SessionData
	if err := sm.sc.Decode("session", cookie.Value, &data); err != nil {
		return nil, err
	}

	return &data, nil
}

// ClearSession removes the session cookie
func (sm *SessionManager) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   sm.isSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
