package storage

import (
	"gopublic/internal/models"
	"log"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var DB *gorm.DB

func InitDB(path string) {
	var err error
	DB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		log.Fatal("failed to connect database")
	}

	// Auto Migrate
	DB.AutoMigrate(&models.User{}, &models.Token{}, &models.Domain{})
}

// Helper for MVP to seed data if empty
func SeedData() {
	var count int64
	DB.Model(&models.User{}).Count(&count)
	if count == 0 {
		log.Println("Seeding test data...")
		user := models.User{Email: "test@example.com"}
		DB.Create(&user)

		token := models.Token{TokenString: "sk_live_12345", UserID: user.ID}
		DB.Create(&token)

		// Assign some default domains
		domains := []string{"misty-river", "silent-star", "bold-eagle"}
		for _, d := range domains {
			DB.Create(&models.Domain{Name: d, UserID: user.ID})
		}
		log.Println("Seeding complete. Use token: sk_live_12345")
	}
}

func ValidateToken(tokenStr string) (*models.User, error) {
	var token models.Token
	result := DB.Preload("User").Where("token_string = ?", tokenStr).First(&token)
	if result.Error != nil {
		return nil, result.Error
	}
	return &token.User, nil
}

func ValidateDomainOwnership(domainName string, userID uint) bool {
	var domain models.Domain
	result := DB.Where("name = ? AND user_id = ?", domainName, userID).First(&domain)
	return result.Error == nil
}

func GetUserDomains(userID uint) []models.Domain {
	var domains []models.Domain
	DB.Where("user_id = ?", userID).Find(&domains)
	return domains
}
