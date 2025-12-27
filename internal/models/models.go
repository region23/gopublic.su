package models

import (
	"time"

	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Email           string
	TelegramID      *int64  `gorm:"uniqueIndex"` // nil if not linked via Telegram
	YandexID        *string `gorm:"uniqueIndex"` // nil if not linked via Yandex
	FirstName       string
	LastName        string
	Username        string
	PhotoURL        string
	TermsAcceptedAt *time.Time // nil if terms not yet accepted
}

type Token struct {
	gorm.Model
	TokenString string `gorm:"uniqueIndex"` // Deprecated: for backward compatibility
	TokenHash   string `gorm:"uniqueIndex"` // SHA256 hash of the token
	UserID      uint
	User        User
}

type Domain struct {
	gorm.Model
	Name   string `gorm:"uniqueIndex"`
	UserID uint
	User   User
}

// AbuseReport stores user reports about malicious tunnels
type AbuseReport struct {
	gorm.Model
	TunnelURL     string // URL of the reported tunnel
	ReportType    string // phishing, malware, spam, other
	Description   string // Description of the issue
	ReporterEmail string // Optional email for contact
	Status        string `gorm:"default:pending"` // pending, reviewed, resolved
}

// UserBandwidth tracks daily bandwidth usage per user
type UserBandwidth struct {
	gorm.Model
	UserID    uint      `gorm:"uniqueIndex:idx_user_date"`
	Date      time.Time `gorm:"uniqueIndex:idx_user_date;type:date"` // Date only (no time)
	BytesUsed int64
}
