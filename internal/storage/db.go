package storage

import (
	"errors"
	"log"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"gopublic/internal/auth"
	apperrors "gopublic/internal/errors"
	"gopublic/internal/models"
)

// Common errors for storage operations.
// These are aliases to the centralized errors package for backward compatibility.
var (
	ErrNotFound     = apperrors.ErrNotFound
	ErrDBError      = apperrors.ErrInternal
	ErrDuplicateKey = apperrors.ErrDuplicateKey
)

// DB is the global database instance.
// Deprecated: Use SQLiteStore via dependency injection instead.
var DB *gorm.DB

// SQLiteStore implements the Store interface using SQLite/GORM
type SQLiteStore struct {
	db *gorm.DB
}

// NewSQLiteStore creates a new SQLite store
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// Configure connection pool
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)

	// Auto Migrate
	if err := db.AutoMigrate(&models.User{}, &models.Token{}, &models.Domain{}); err != nil {
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

// Close closes the database connection
func (s *SQLiteStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// GetDB returns the underlying GORM DB (for backward compatibility)
func (s *SQLiteStore) GetDB() *gorm.DB {
	return s.db
}

// --- User Operations ---

func (s *SQLiteStore) GetUserByID(id uint) (*models.User, error) {
	var user models.User
	result := s.db.First(&user, id)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, result.Error
	}
	return &user, nil
}

func (s *SQLiteStore) GetUserByTelegramID(telegramID int64) (*models.User, error) {
	var user models.User
	result := s.db.Where("telegram_id = ?", telegramID).First(&user)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, result.Error
	}
	return &user, nil
}

func (s *SQLiteStore) CreateUser(user *models.User) error {
	return s.db.Create(user).Error
}

func (s *SQLiteStore) UpdateUser(user *models.User) error {
	return s.db.Save(user).Error
}

// --- Token Operations ---

func (s *SQLiteStore) ValidateToken(tokenStr string) (*models.User, error) {
	var token models.Token

	// First try new hash-based lookup
	tokenHash := auth.HashToken(tokenStr)
	result := s.db.Preload("User").Where("token_hash = ?", tokenHash).First(&token)
	if result.Error == nil {
		return &token.User, nil
	}

	// Fallback to legacy plaintext lookup for backward compatibility
	result = s.db.Preload("User").Where("token_string = ?", tokenStr).First(&token)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, result.Error
	}
	return &token.User, nil
}

func (s *SQLiteStore) GetUserToken(userID uint) (*models.Token, error) {
	var token models.Token
	result := s.db.Where("user_id = ?", userID).First(&token)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, result.Error
	}
	return &token, nil
}

func (s *SQLiteStore) CreateToken(token *models.Token) error {
	return s.db.Create(token).Error
}

// RegenerateToken creates a new token for the user, replacing the old one.
// Returns the new token string (shown only once to user).
func (s *SQLiteStore) RegenerateToken(userID uint) (string, error) {
	var tokenString string

	err := s.db.Transaction(func(tx *gorm.DB) error {
		// Delete existing token
		if err := tx.Where("user_id = ?", userID).Delete(&models.Token{}).Error; err != nil {
			return err
		}

		// Generate new token
		var err error
		tokenString, err = auth.GenerateSecureToken()
		if err != nil {
			return err
		}

		// Create new token
		token := models.Token{
			TokenString: tokenString,
			TokenHash:   auth.HashToken(tokenString),
			UserID:      userID,
		}
		return tx.Create(&token).Error
	})

	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// --- Domain Operations ---

func (s *SQLiteStore) GetUserDomains(userID uint) ([]models.Domain, error) {
	var domains []models.Domain
	if err := s.db.Where("user_id = ?", userID).Find(&domains).Error; err != nil {
		return nil, err
	}
	return domains, nil
}

func (s *SQLiteStore) ValidateDomainOwnership(domainName string, userID uint) (bool, error) {
	var domain models.Domain
	result := s.db.Where("name = ? AND user_id = ?", domainName, userID).First(&domain)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, result.Error
	}
	return true, nil
}

func (s *SQLiteStore) CreateDomain(domain *models.Domain) error {
	return s.db.Create(domain).Error
}

// --- Transaction Operations ---

// UserRegistration holds data for creating a new user with token and domains
type UserRegistration struct {
	User    *models.User
	Domains []string
}

// CreateUserWithTokenAndDomains creates a user, token, and domains in a single transaction.
// Returns the created user and token string (shown only once to user).
func (s *SQLiteStore) CreateUserWithTokenAndDomains(reg UserRegistration) (*models.User, string, error) {
	var tokenString string

	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 1. Create user
		if err := tx.Create(reg.User).Error; err != nil {
			return err
		}

		// 2. Generate and create token
		var err error
		tokenString, err = auth.GenerateSecureToken()
		if err != nil {
			return err
		}

		token := models.Token{
			TokenString: tokenString,
			TokenHash:   auth.HashToken(tokenString),
			UserID:      reg.User.ID,
		}
		if err := tx.Create(&token).Error; err != nil {
			return err
		}

		// 3. Create domains
		for _, name := range reg.Domains {
			domain := models.Domain{Name: name, UserID: reg.User.ID}
			if err := tx.Create(&domain).Error; err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, "", err
	}

	return reg.User, tokenString, nil
}

// --- Seeding ---

// SeedData seeds test data for development
func (s *SQLiteStore) SeedData() {
	var count int64
	s.db.Model(&models.User{}).Count(&count)
	if count == 0 {
		log.Println("Seeding test data...")
		user := models.User{Email: "test@example.com"}
		s.db.Create(&user)

		token := models.Token{TokenString: "sk_live_12345", UserID: user.ID}
		s.db.Create(&token)

		// Assign some default domains
		domains := []string{"misty-river", "silent-star", "bold-eagle"}
		for _, d := range domains {
			s.db.Create(&models.Domain{Name: d, UserID: user.ID})
		}
		log.Println("Seeding complete. Use token: sk_live_12345")
	}
}

// =============================================================================
// Backward Compatibility Layer
// These package-level functions use the global DB variable.
// Deprecated: Migrate to using SQLiteStore directly.
// =============================================================================

// InitDB initializes the global database connection.
// Deprecated: Use NewSQLiteStore instead.
func InitDB(path string) error {
	store, err := NewSQLiteStore(path)
	if err != nil {
		return err
	}
	DB = store.db
	return nil
}

// SeedData seeds test data using the global DB.
// Deprecated: Use SQLiteStore.SeedData instead.
func SeedData() {
	if DB == nil {
		return
	}
	(&SQLiteStore{db: DB}).SeedData()
}

// ValidateToken validates a token using the global DB.
// Deprecated: Use SQLiteStore.ValidateToken instead.
func ValidateToken(tokenStr string) (*models.User, error) {
	if DB == nil {
		return nil, ErrDBError
	}
	return (&SQLiteStore{db: DB}).ValidateToken(tokenStr)
}

// ValidateDomainOwnership checks domain ownership using the global DB.
// Deprecated: Use SQLiteStore.ValidateDomainOwnership instead.
func ValidateDomainOwnership(domainName string, userID uint) (bool, error) {
	if DB == nil {
		return false, ErrDBError
	}
	return (&SQLiteStore{db: DB}).ValidateDomainOwnership(domainName, userID)
}

// GetUserDomains gets user domains using the global DB.
// Deprecated: Use SQLiteStore.GetUserDomains instead.
func GetUserDomains(userID uint) ([]models.Domain, error) {
	if DB == nil {
		return nil, ErrDBError
	}
	return (&SQLiteStore{db: DB}).GetUserDomains(userID)
}

// CreateUserWithTokenAndDomains creates user with token and domains using the global DB.
// Deprecated: Use SQLiteStore.CreateUserWithTokenAndDomains instead.
func CreateUserWithTokenAndDomains(reg UserRegistration) (*models.User, string, error) {
	if DB == nil {
		return nil, "", ErrDBError
	}
	return (&SQLiteStore{db: DB}).CreateUserWithTokenAndDomains(reg)
}

// GetUserByTelegramID gets user by Telegram ID using the global DB.
// Deprecated: Use SQLiteStore.GetUserByTelegramID instead.
func GetUserByTelegramID(telegramID int64) (*models.User, error) {
	if DB == nil {
		return nil, ErrDBError
	}
	return (&SQLiteStore{db: DB}).GetUserByTelegramID(telegramID)
}

// UpdateUser updates a user using the global DB.
// Deprecated: Use SQLiteStore.UpdateUser instead.
func UpdateUser(user *models.User) error {
	if DB == nil {
		return ErrDBError
	}
	return (&SQLiteStore{db: DB}).UpdateUser(user)
}

// GetUserToken gets user token using the global DB.
// Deprecated: Use SQLiteStore.GetUserToken instead.
func GetUserToken(userID uint) (*models.Token, error) {
	if DB == nil {
		return nil, ErrDBError
	}
	return (&SQLiteStore{db: DB}).GetUserToken(userID)
}

// GetUserByID gets user by ID using the global DB.
// Deprecated: Use SQLiteStore.GetUserByID instead.
func GetUserByID(id uint) (*models.User, error) {
	if DB == nil {
		return nil, ErrDBError
	}
	return (&SQLiteStore{db: DB}).GetUserByID(id)
}

// RegenerateToken regenerates a user's token using the global DB.
// Deprecated: Use SQLiteStore.RegenerateToken instead.
func RegenerateToken(userID uint) (string, error) {
	if DB == nil {
		return "", ErrDBError
	}
	return (&SQLiteStore{db: DB}).RegenerateToken(userID)
}
