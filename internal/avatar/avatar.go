package avatar

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	// AvatarDir is the directory where avatars are stored
	AvatarDir = "data/avatars"

	// DefaultTimeout for downloading avatars
	DefaultTimeout = 5 * time.Second
)

// Init creates the avatar directory if it doesn't exist
func Init() error {
	return os.MkdirAll(AvatarDir, 0755)
}

// GetPath returns the file path for a user's avatar
func GetPath(userID uint) string {
	return filepath.Join(AvatarDir, fmt.Sprintf("%d.jpg", userID))
}

// GetURL returns the URL path for serving a user's avatar
func GetURL(userID uint) string {
	return fmt.Sprintf("/avatars/%d.jpg", userID)
}

// Exists checks if an avatar file exists for the given user
func Exists(userID uint) bool {
	_, err := os.Stat(GetPath(userID))
	return err == nil
}

// Download fetches an avatar from a URL and saves it locally
// Returns the local URL path on success, empty string on failure
func Download(userID uint, remoteURL string) string {
	if remoteURL == "" {
		return ""
	}

	// Ensure directory exists
	if err := Init(); err != nil {
		log.Printf("Failed to create avatar directory: %v", err)
		return ""
	}

	// Download with timeout
	client := &http.Client{Timeout: DefaultTimeout}
	resp, err := client.Get(remoteURL)
	if err != nil {
		log.Printf("Failed to download avatar for user %d: %v", userID, err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Failed to download avatar for user %d: status %d", userID, resp.StatusCode)
		return ""
	}

	// Create file
	filePath := GetPath(userID)
	file, err := os.Create(filePath)
	if err != nil {
		log.Printf("Failed to create avatar file for user %d: %v", userID, err)
		return ""
	}
	defer file.Close()

	// Copy data
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		log.Printf("Failed to save avatar for user %d: %v", userID, err)
		os.Remove(filePath) // Clean up partial file
		return ""
	}

	log.Printf("Downloaded avatar for user %d", userID)
	return GetURL(userID)
}

// Delete removes a user's avatar file
func Delete(userID uint) error {
	path := GetPath(userID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // Already doesn't exist
	}
	return os.Remove(path)
}
