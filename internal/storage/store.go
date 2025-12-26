package storage

import (
	"time"

	"gopublic/internal/models"
)

// Store defines the interface for data persistence operations.
// This allows for easy testing with mock implementations and
// potential future support for different storage backends.
type Store interface {
	// User operations
	GetUserByID(id uint) (*models.User, error)
	GetUserByTelegramID(telegramID int64) (*models.User, error)
	GetUserByYandexID(yandexID string) (*models.User, error)
	CreateUser(user *models.User) error
	UpdateUser(user *models.User) error
	AcceptTerms(userID uint) error
	LinkYandexAccount(userID uint, yandexID string) error
	LinkTelegramAccount(userID uint, telegramID int64) error

	// Token operations
	ValidateToken(tokenStr string) (*models.User, error)
	GetUserToken(userID uint) (*models.Token, error)
	CreateToken(token *models.Token) error
	RegenerateToken(userID uint) (string, error)

	// Domain operations
	GetUserDomains(userID uint) ([]models.Domain, error)
	ValidateDomainOwnership(domainName string, userID uint) (bool, error)
	CreateDomain(domain *models.Domain) error

	// Abuse report operations
	CreateAbuseReport(report *models.AbuseReport) error
	GetAbuseReports(status string) ([]models.AbuseReport, error)

	// Bandwidth operations
	GetUserBandwidthToday(userID uint) (int64, error)
	AddUserBandwidth(userID uint, bytes int64) error

	// Transaction support
	CreateUserWithTokenAndDomains(reg UserRegistration) (*models.User, string, error)

	// Lifecycle
	Close() error
}

// BandwidthResult holds the result of a bandwidth check
type BandwidthResult struct {
	BytesUsed int64
	Date      time.Time
}

// Ensure SQLiteStore implements Store interface
var _ Store = (*SQLiteStore)(nil)
