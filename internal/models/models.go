package models

import (
	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Email string `gorm:"uniqueIndex"`
}

type Token struct {
	gorm.Model
	TokenString string `gorm:"uniqueIndex"`
	UserID      uint
	User        User
}

type Domain struct {
	gorm.Model
	Name   string `gorm:"uniqueIndex"`
	UserID uint
	User   User
}
