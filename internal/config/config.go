package config

import (
	"encoding/hex"
	"os"
	"strconv"

	apperrors "gopublic/internal/errors"
)

// Config holds all server configuration
type Config struct {
	// Server settings
	Domain       string // Root domain (e.g., "example.com")
	ProjectName  string // Project name for branding (default: "Go Public")
	Email        string // Email for Let's Encrypt
	InsecureMode bool   // If true, use HTTP instead of HTTPS
	DBPath       string // Path to SQLite database

	// Control plane settings
	ControlPlanePort string // Port for control plane (default ":4443")
	MaxConnections   int    // Max concurrent tunnel connections

	// Telegram OAuth
	TelegramBotToken string
	TelegramBotName  string

	// GitHub repository for client downloads (e.g., "username/gopublic")
	GitHubRepo string

	// Number of domains to assign per new user (default: 2)
	DomainsPerUser int

	// Session keys (32 bytes each)
	SessionHashKey  []byte
	SessionBlockKey []byte
}

// Configuration errors
var (
	ErrMissingDomain      = apperrors.New(apperrors.CodeConfigError, "DOMAIN_NAME is required in production mode")
	ErrMissingSessionKeys = apperrors.New(apperrors.CodeConfigError, "SESSION_HASH_KEY and SESSION_BLOCK_KEY are required in production mode")
	ErrInvalidSessionKey  = apperrors.New(apperrors.CodeConfigError, "session key must be 32 bytes hex-encoded")
)

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv() (*Config, error) {
	// Parse domains per user (default: 2)
	domainsPerUser := 2
	if val := os.Getenv("DOMAINS_PER_USER"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			domainsPerUser = n
		}
	}

	cfg := &Config{
		Domain:           os.Getenv("DOMAIN_NAME"),
		ProjectName:      getEnvOrDefault("PROJECT_NAME", "Go Public"),
		Email:            os.Getenv("EMAIL"),
		InsecureMode:     os.Getenv("INSECURE_HTTP") == "true",
		DBPath:           getEnvOrDefault("DB_PATH", "gopublic.db"),
		ControlPlanePort: getEnvOrDefault("CONTROL_PLANE_PORT", ":4443"),
		MaxConnections:   1000,
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramBotName:  os.Getenv("TELEGRAM_BOT_NAME"),
		GitHubRepo:       os.Getenv("GITHUB_REPO"),
		DomainsPerUser:   domainsPerUser,
	}

	// Parse session keys
	if hashKeyHex := os.Getenv("SESSION_HASH_KEY"); hashKeyHex != "" {
		key, err := hex.DecodeString(hashKeyHex)
		if err != nil || len(key) < 32 {
			return nil, ErrInvalidSessionKey
		}
		cfg.SessionHashKey = key[:32]
	}

	if blockKeyHex := os.Getenv("SESSION_BLOCK_KEY"); blockKeyHex != "" {
		key, err := hex.DecodeString(blockKeyHex)
		if err != nil || len(key) < 32 {
			return nil, ErrInvalidSessionKey
		}
		cfg.SessionBlockKey = key[:32]
	}

	return cfg, nil
}

// Validate checks if the configuration is valid for the given mode
func (c *Config) Validate() error {
	// In production mode (not insecure), require domain and session keys
	if !c.InsecureMode && !c.IsLocalDev() {
		if c.Domain == "" {
			return ErrMissingDomain
		}
		if c.SessionHashKey == nil || c.SessionBlockKey == nil {
			return ErrMissingSessionKeys
		}
	}

	// Telegram config is optional (dashboard won't work without it)
	// but we don't fail startup

	return nil
}

// IsLocalDev returns true if running in local development mode
func (c *Config) IsLocalDev() bool {
	return c.Domain == "" || c.Domain == "localhost" || c.Domain == "127.0.0.1"
}

// IsSecure returns true if HTTPS should be used
func (c *Config) IsSecure() bool {
	return !c.InsecureMode && !c.IsLocalDev()
}

// IngressPort returns the appropriate ingress port based on mode
func (c *Config) IngressPort() string {
	if c.InsecureMode {
		return ":80"
	}
	return ":8080"
}

// AllowInsecureSessionKeys returns true if random session keys are allowed
func (c *Config) AllowInsecureSessionKeys() bool {
	return c.InsecureMode || c.IsLocalDev()
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
