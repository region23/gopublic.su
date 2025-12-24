package updater

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// PublicKeyBase64 is set via ldflags during build
var PublicKeyBase64 = ""

// GitHubRepo is the repository to check for updates
var GitHubRepo = "region23/gopublic.su"

// Release represents a GitHub release
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a release asset
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// UpdateInfo contains information about an available update
type UpdateInfo struct {
	Available      bool
	CurrentVersion string
	LatestVersion  string
	DownloadURL    string
	AssetName      string
}

// UpdateResult represents the result of an update operation
type UpdateResult struct {
	Success       bool
	Message       string
	NeedsRestart  bool
	PendingUpdate bool // Windows: update downloaded but needs manual restart
}

// httpClient with reasonable timeouts
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

// CheckForUpdate checks GitHub for a newer version
func CheckForUpdate(ctx context.Context, currentVersion string) (*UpdateInfo, error) {
	info := &UpdateInfo{
		CurrentVersion: currentVersion,
	}

	// Skip check for dev versions
	if strings.HasPrefix(currentVersion, "dev") {
		return info, nil
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", GitHubRepo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "gopublic-client")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No releases yet
		return info, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release: %w", err)
	}

	info.LatestVersion = release.TagName

	// Compare versions (simple: if different and latest is not empty, update available)
	if release.TagName != "" && release.TagName != currentVersion {
		// Find the right asset for this platform
		assetName := getAssetName()
		for _, asset := range release.Assets {
			if asset.Name == assetName {
				info.Available = true
				info.DownloadURL = asset.BrowserDownloadURL
				info.AssetName = asset.Name
				break
			}
		}
	}

	return info, nil
}

// getAssetName returns the expected asset name for the current platform
func getAssetName() string {
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "arm64" {
			return "gopublic-linux-arm64"
		}
		return "gopublic-linux-amd64"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "gopublic-macos-arm64"
		}
		return "gopublic-macos-amd64"
	case "windows":
		return "gopublic-windows-amd64.exe"
	default:
		return ""
	}
}

// PerformUpdate downloads and installs the update
func PerformUpdate(ctx context.Context, info *UpdateInfo) (*UpdateResult, error) {
	if PublicKeyBase64 == "" {
		return nil, fmt.Errorf("update verification not configured (no public key)")
	}

	pubKeyBytes, err := base64.StdEncoding.DecodeString(PublicKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key size")
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	// Construct base URL from download URL
	baseURL := strings.TrimSuffix(info.DownloadURL, info.AssetName)

	// Download checksums.txt and checksums.sig
	checksums, err := downloadFile(ctx, baseURL+"checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("failed to download checksums: %w", err)
	}

	signature, err := downloadFile(ctx, baseURL+"checksums.sig")
	if err != nil {
		return nil, fmt.Errorf("failed to download signature: %w", err)
	}

	// Verify signature
	if !ed25519.Verify(pubKey, checksums, signature) {
		return nil, fmt.Errorf("signature verification failed - update rejected")
	}

	// Parse expected checksum for our asset
	expectedHash, err := parseChecksum(checksums, info.AssetName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse checksum: %w", err)
	}

	// Download the binary
	binaryData, err := downloadFile(ctx, info.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download binary: %w", err)
	}

	// Verify checksum
	actualHash := sha256.Sum256(binaryData)
	actualHashHex := hex.EncodeToString(actualHash[:])
	if actualHashHex != expectedHash {
		return nil, fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHashHex)
	}

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Platform-specific installation
	if runtime.GOOS == "windows" {
		return installWindows(execPath, binaryData)
	}
	return installUnix(execPath, binaryData)
}

// downloadFile downloads a file with retries
func downloadFile(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "gopublic-client")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		return data, nil
	}
	return nil, fmt.Errorf("download failed after 3 attempts: %w", lastErr)
}

// parseChecksum extracts the checksum for a specific file from checksums.txt
func parseChecksum(data []byte, filename string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		// Format: "hash  filename" or "hash filename"
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			hash := parts[0]
			name := parts[len(parts)-1]
			if name == filename {
				return hash, nil
			}
		}
	}
	return "", fmt.Errorf("checksum not found for %s", filename)
}

// installUnix performs atomic replacement on Unix systems
func installUnix(execPath string, data []byte) (*UpdateResult, error) {
	// Write to temp file in same directory (for atomic rename)
	dir := filepath.Dir(execPath)
	tmpFile, err := os.CreateTemp(dir, "gopublic-update-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to write update: %w", err)
	}
	tmpFile.Close()

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to set permissions: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to install update: %w", err)
	}

	return &UpdateResult{
		Success:      true,
		Message:      "Update installed successfully. Restart to apply.",
		NeedsRestart: true,
	}, nil
}

// installWindows handles Windows-specific update (can't replace running exe)
func installWindows(execPath string, data []byte) (*UpdateResult, error) {
	// Write new version next to current with .new extension
	newPath := execPath + ".new"

	if err := os.WriteFile(newPath, data, 0755); err != nil {
		return nil, fmt.Errorf("failed to write update: %w", err)
	}

	// Create a batch script to replace the exe on next run
	batchPath := execPath + ".update.bat"
	batchContent := fmt.Sprintf(`@echo off
:retry
timeout /t 1 /nobreak >nul
del "%s" 2>nul
if exist "%s" goto retry
move "%s" "%s"
del "%%~f0"
`, execPath, execPath, newPath, execPath)

	if err := os.WriteFile(batchPath, []byte(batchContent), 0755); err != nil {
		os.Remove(newPath)
		return nil, fmt.Errorf("failed to create update script: %w", err)
	}

	return &UpdateResult{
		Success:       true,
		Message:       "Update downloaded. Close the app and run " + filepath.Base(batchPath) + " to complete.",
		NeedsRestart:  true,
		PendingUpdate: true,
	}, nil
}
