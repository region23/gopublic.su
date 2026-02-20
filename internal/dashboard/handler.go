package dashboard

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gopublic/internal/auth"
	"gopublic/internal/avatar"
	"gopublic/internal/config"
	"gopublic/internal/metrics"
	"gopublic/internal/models"
	"gopublic/internal/sentry"
	"gopublic/internal/storage"
	"gopublic/internal/telegram"
	"gopublic/internal/version"
)

//go:embed templates/*
var templateFS embed.FS

// UserSessionProvider provides information about active user sessions.
// This interface is implemented by server.UserSessionRegistry.
type UserSessionProvider interface {
	IsConnected(userID uint) bool
	GetActiveDomains(userID uint) []string
}

type Handler struct {
	BotToken              string
	BotName               string
	Domain                string
	GitHubRepo            string
	DomainsPerUser        int
	DailyBandwidthLimit   int64 // in bytes
	AdminTelegramID       int64
	YandexClientID        string
	YandexClientSecret    string
	Session               *auth.SessionManager
	UserSessions          UserSessionProvider // Optional: provides active session info
	TelegramBot           *telegram.Bot       // Telegram bot for auth
	TelegramWidgetEnabled bool                // If true, use legacy Telegram Login Widget
	AppMetrics            *metrics.AppMetrics // Optional: Prometheus metrics
	MetricsToken          string              // Optional: Bearer token for /metrics endpoint
}

// SetUserSessions sets the user session provider for displaying connection status.
func (h *Handler) SetUserSessions(provider UserSessionProvider) {
	h.UserSessions = provider
}

// NewHandlerWithConfig creates a new dashboard handler with the given configuration.
func NewHandlerWithConfig(cfg *config.Config) (*Handler, error) {
	sessionCfg := auth.SessionConfig{
		IsSecure:          cfg.IsSecure(),
		AllowInsecureKeys: cfg.AllowInsecureSessionKeys(),
	}

	sessionMgr, err := auth.NewSessionManager(sessionCfg)
	if err != nil {
		return nil, err
	}

	return &Handler{
		BotToken:            cfg.TelegramBotToken,
		BotName:             cfg.TelegramBotName,
		Domain:              cfg.Domain,
		GitHubRepo:          cfg.GitHubRepo,
		DomainsPerUser:      cfg.DomainsPerUser,
		DailyBandwidthLimit: cfg.DailyBandwidthLimit,
		AdminTelegramID:     cfg.AdminTelegramID,
		YandexClientID:      cfg.YandexClientID,
		YandexClientSecret:  cfg.YandexClientSecret,
		Session:             sessionMgr,
	}, nil
}

// NewHandler creates a new dashboard handler using environment variables.
// Deprecated: Use NewHandlerWithConfig instead.
func NewHandler() (*Handler, error) {
	domain := os.Getenv("DOMAIN_NAME")
	isSecure := domain != "" && domain != "localhost" && domain != "127.0.0.1"

	sessionCfg := auth.SessionConfig{
		IsSecure:          isSecure,
		AllowInsecureKeys: !isSecure,
	}

	sessionMgr, err := auth.NewSessionManager(sessionCfg)
	if err != nil {
		return nil, err
	}

	return &Handler{
		BotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		BotName:  os.Getenv("TELEGRAM_BOT_NAME"),
		Domain:   domain,
		Session:  sessionMgr,
	}, nil
}

func (h *Handler) LoadTemplates(r *gin.Engine) error {
	// Define template functions
	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"formatBytes": func(bytes int64) string {
			if bytes < 1024 {
				return fmt.Sprintf("%d B", bytes)
			}
			if bytes < 1024*1024 {
				return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
			}
			if bytes < 1024*1024*1024 {
				return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
			}
			return fmt.Sprintf("%.2f GB", float64(bytes)/(1024*1024*1024))
		},
		"bandwidthPercent": func(used, limit int64) int {
			if limit == 0 {
				return 0
			}
			pct := int(float64(used) / float64(limit) * 100)
			if pct > 100 {
				return 100
			}
			return pct
		},
	}

	// Parse templates with functions
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	r.SetHTMLTemplate(tmpl)
	return nil
}

// Deprecated: Routes are now handled manually in ingress.go
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	if err := h.LoadTemplates(r); err != nil {
		log.Fatal("Failed to parse templates:", err)
	}

	g := r.Group("/")
	g.GET("/", h.Index)
	g.GET("/login", h.Login)
	g.GET("/auth/telegram", h.TelegramCallback)
	g.GET("/logout", h.Logout)

	if h.AppMetrics != nil {
		g.GET("/metrics", h.Metrics)
	}
}

func (h *Handler) Login(c *gin.Context) {
	// If already logged in, redirect to index
	if _, err := h.getUserFromSession(c); err == nil {
		c.Redirect(http.StatusTemporaryRedirect, "/")
		return
	}

	var authURL string
	if h.Domain == "localhost" || h.Domain == "127.0.0.1" {
		authURL = fmt.Sprintf("http://%s/auth/telegram", h.Domain)
	} else {
		authURL = fmt.Sprintf("https://app.%s/auth/telegram", h.Domain)
	}

	c.HTML(http.StatusOK, "login.html", gin.H{
		"BotName":               h.BotName,
		"AuthURL":               authURL,
		"GitHubRepo":            h.GitHubRepo,
		"Version":               version.Version,
		"YandexEnabled":         h.YandexClientID != "" && h.YandexClientSecret != "",
		"TelegramBotEnabled":    h.BotName != "" && h.BotToken != "",
		"TelegramWidgetEnabled": h.TelegramWidgetEnabled,
	})
}

func (h *Handler) Index(c *gin.Context) {
	user, err := h.getUserFromSession(c)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login")
		return
	}

	// Fetch token
	token, err := storage.GetUserToken(user.ID)
	if err != nil {
		sentry.CaptureErrorWithContextf(c, err, "Failed to fetch token for user %d", user.ID)
		c.String(http.StatusInternalServerError, "Failed to load user data")
		return
	}

	// Fetch domains
	domains, err := storage.GetUserDomains(user.ID)
	if err != nil {
		sentry.CaptureErrorWithContextf(c, err, "Failed to fetch domains for user %d", user.ID)
		c.String(http.StatusInternalServerError, "Failed to load user data")
		return
	}

	// Fetch bandwidth statistics
	bandwidthToday, _ := storage.GetUserBandwidthToday(user.ID)
	bandwidthTotal, _ := storage.GetUserTotalBandwidth(user.ID)
	bandwidthLimit := h.DailyBandwidthLimit
	if h.AdminTelegramID != 0 && user.TelegramID != nil && *user.TelegramID == h.AdminTelegramID {
		bandwidthLimit = 0
	}

	// Check connection status
	var isConnected bool
	var activeDomains []string
	if h.UserSessions != nil {
		isConnected = h.UserSessions.IsConnected(user.ID)
		activeDomains = h.UserSessions.GetActiveDomains(user.ID)
	}

	c.HTML(http.StatusOK, "index.html", gin.H{
		"User":            user,
		"Token":           token.TokenString,
		"Domains":         domains,
		"RootDomain":      h.Domain,
		"GitHubRepo":      h.GitHubRepo,
		"Version":         version.Version,
		"TermsAccepted":   user.TermsAcceptedAt != nil,
		"TelegramEnabled": h.BotToken != "" && h.BotName != "",
		"YandexEnabled":   h.YandexClientID != "" && h.YandexClientSecret != "",
		"BandwidthToday":  bandwidthToday,
		"BandwidthTotal":  bandwidthTotal,
		"BandwidthLimit":  bandwidthLimit,
		"IsConnected":     isConnected,
		"ActiveDomains":   activeDomains,
	})
}

func (h *Handler) TelegramCallback(c *gin.Context) {
	// Verify Hash
	if !h.verifyTelegramHash(c.Request.URL.Query()) {
		c.String(http.StatusUnauthorized, "Invalid Telegram Hash")
		return
	}

	data := c.Request.URL.Query()
	idStr := data.Get("id")
	var tgID int64
	fmt.Sscanf(idStr, "%d", &tgID)
	firstName := data.Get("first_name")
	lastName := data.Get("last_name")
	username := data.Get("username")
	photoURL := data.Get("photo_url")

	log.Printf("Telegram callback: id=%s, first_name=%s, photo_url=%s", idStr, firstName, photoURL)

	// Find or Create User
	user, err := storage.GetUserByTelegramID(tgID)

	if err == storage.ErrNotFound {
		// Create new user with token and domains in a single transaction
		newUser := &models.User{
			TelegramID: &tgID,
			FirstName:  firstName,
			LastName:   lastName,
			Username:   username,
		}

		// Generate domain names
		prefixes := []string{"misty", "silent", "bold", "rapid", "cool"}
		suffixes := []string{"river", "star", "eagle", "bear", "fox"}
		var domains []string
		for i := 0; i < h.DomainsPerUser; i++ {
			name := fmt.Sprintf("%s-%s-%d", prefixes[i%len(prefixes)], suffixes[i%len(suffixes)], time.Now().Unix()%1000+int64(i))
			domains = append(domains, name)
		}

		reg := storage.UserRegistration{
			User:    newUser,
			Domains: domains,
		}

		createdUser, _, err := storage.CreateUserWithTokenAndDomains(reg)
		if err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to create user")
			c.String(http.StatusInternalServerError, "Failed to create user account")
			return
		}
		user = createdUser
		if h.AppMetrics != nil {
			h.AppMetrics.UserCreated()
		}

		// Download and save avatar locally
		if photoURL != "" {
			if localURL := avatar.Download(user.ID, photoURL); localURL != "" {
				user.PhotoURL = localURL
				storage.UpdateUser(user)
			}
		}
	} else if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Database error looking up user")
		c.String(http.StatusInternalServerError, "Database error")
		return
	} else {
		// Update existing user info
		user.FirstName = firstName
		user.LastName = lastName
		user.Username = username

		// Download and save avatar locally
		if photoURL != "" {
			if localURL := avatar.Download(user.ID, photoURL); localURL != "" {
				user.PhotoURL = localURL
			}
		}

		if err := storage.UpdateUser(user); err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to update user")
			c.String(http.StatusInternalServerError, "Failed to update user")
			return
		}
	}

	// Set secure signed session cookie
	if err := h.Session.SetSession(c.Writer, user.ID); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to set session")
		c.String(http.StatusInternalServerError, "Failed to create session")
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, "/")
}

func (h *Handler) Logout(c *gin.Context) {
	h.Session.ClearSession(c.Writer)
	c.Redirect(http.StatusTemporaryRedirect, "/login")
}

// RegenerateToken handles POST /api/regenerate-token - creates a new token for the user
func (h *Handler) RegenerateToken(c *gin.Context) {
	// Validate CSRF token (double-submit cookie pattern)
	cookieToken, err := c.Cookie("csrf_token")
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token missing"})
		return
	}

	requestToken := c.GetHeader("X-CSRF-Token")
	if requestToken == "" || requestToken != cookieToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token invalid"})
		return
	}

	// Validate session
	user, err := h.getUserFromSession(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	newToken, err := storage.RegenerateToken(user.ID)
	if err != nil {
		sentry.CaptureErrorWithContextf(c, err, "Failed to regenerate token for user %d", user.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to regenerate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":   newToken,
		"command": fmt.Sprintf("gopublic auth %s", newToken),
	})
}

func (h *Handler) getUserFromSession(c *gin.Context) (*models.User, error) {
	session, err := h.Session.GetSession(c.Request)
	if err != nil {
		return nil, err
	}

	return storage.GetUserByID(session.UserID)
}

func (h *Handler) verifyTelegramHash(params map[string][]string) bool {
	// See: https://core.telegram.org/widgets/login#checking-authorization
	token := h.BotToken
	if token == "" {
		log.Println("TELEGRAM_BOT_TOKEN not set")
		return false
	}

	checkHash := params["hash"][0]
	delete(params, "hash")

	var keys []string
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, params[k][0]))
	}
	dataCheckString := strings.Join(parts, "\n")

	// SHA256(botToken)
	sha256Token := sha256.Sum256([]byte(token))

	// HMAC-SHA256(dataCheckString)
	hmacHash := hmac.New(sha256.New, sha256Token[:])
	hmacHash.Write([]byte(dataCheckString))
	calculatedHash := hex.EncodeToString(hmacHash.Sum(nil))

	// Restore hash map for subsequent use (if any framework reused it, but here it's query copy-ish)
	// Actually URL.Query() returns copy? No. But we don't need it anymore.

	return calculatedHash == checkHash
}

// Terms displays the Terms of Service page
func (h *Handler) Terms(c *gin.Context) {
	c.HTML(http.StatusOK, "terms.html", gin.H{
		"GitHubRepo":            h.GitHubRepo,
		"Version":               version.Version,
		"LastUpdated":           "26 декабря 2025",
		"DailyBandwidthLimitMB": h.DailyBandwidthLimit / (1024 * 1024),
		"DomainsPerUser":        h.DomainsPerUser,
	})
}

// AcceptTerms handles the terms acceptance API
func (h *Handler) AcceptTerms(c *gin.Context) {
	// Validate CSRF
	cookieToken, err := c.Cookie("csrf_token")
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token missing"})
		return
	}

	requestToken := c.GetHeader("X-CSRF-Token")
	if requestToken == "" || requestToken != cookieToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token invalid"})
		return
	}

	// Validate session
	user, err := h.getUserFromSession(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	if err := storage.AcceptTerms(user.ID); err != nil {
		sentry.CaptureErrorWithContextf(c, err, "Failed to accept terms for user %d", user.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to accept terms"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// AbuseForm displays the abuse report form
func (h *Handler) AbuseForm(c *gin.Context) {
	c.HTML(http.StatusOK, "abuse.html", gin.H{
		"GitHubRepo": h.GitHubRepo,
		"Version":    version.Version,
	})
}

// AbuseReportRequest represents the abuse report submission
type AbuseReportRequest struct {
	TunnelURL     string `json:"tunnel_url"`
	ReportType    string `json:"report_type"`
	Description   string `json:"description"`
	ReporterEmail string `json:"reporter_email"`
}

// SubmitAbuseReport handles abuse report submissions
func (h *Handler) SubmitAbuseReport(c *gin.Context) {
	// Validate CSRF
	cookieToken, err := c.Cookie("csrf_token")
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token missing"})
		return
	}

	requestToken := c.GetHeader("X-CSRF-Token")
	if requestToken == "" || requestToken != cookieToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token invalid"})
		return
	}

	var req AbuseReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Validate required fields
	if req.TunnelURL == "" || req.ReportType == "" || req.Description == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing required fields"})
		return
	}

	// Create abuse report
	report := &models.AbuseReport{
		TunnelURL:     req.TunnelURL,
		ReportType:    req.ReportType,
		Description:   req.Description,
		ReporterEmail: req.ReporterEmail,
		Status:        "pending",
	}

	if err := storage.CreateAbuseReport(report); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to create abuse report")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to submit report"})
		return
	}

	// Send Telegram notification to admin
	h.sendAbuseNotification(report)

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// sendAbuseNotification sends a Telegram message to the admin about the abuse report
func (h *Handler) sendAbuseNotification(report *models.AbuseReport) {
	if h.AdminTelegramID == 0 || h.BotToken == "" {
		return
	}

	reportTypes := map[string]string{
		"phishing": "Фишинг",
		"malware":  "Вредоносное ПО",
		"spam":     "Спам",
		"illegal":  "Нелегальный контент",
		"other":    "Другое",
	}

	reportTypeName := reportTypes[report.ReportType]
	if reportTypeName == "" {
		reportTypeName = report.ReportType
	}

	message := fmt.Sprintf(
		"🚨 *Новая жалоба на нарушение*\n\n"+
			"*URL:* %s\n"+
			"*Тип:* %s\n"+
			"*Описание:* %s",
		report.TunnelURL,
		reportTypeName,
		report.Description,
	)

	if report.ReporterEmail != "" {
		message += fmt.Sprintf("\n*Email:* %s", report.ReporterEmail)
	}

	// Send via Telegram Bot API
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", h.BotToken)

	payload := map[string]interface{}{
		"chat_id":    h.AdminTelegramID,
		"text":       message,
		"parse_mode": "Markdown",
	}

	go func() {
		jsonData, _ := json.Marshal(payload)
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("Failed to send Telegram notification: %v", err)
			return
		}
		defer resp.Body.Close()
	}()
}

// YandexUserInfo represents user info from Yandex OAuth
type YandexUserInfo struct {
	ID              string `json:"id"`
	Login           string `json:"login"`
	DefaultEmail    string `json:"default_email"`
	FirstName       string `json:"first_name"`
	LastName        string `json:"last_name"`
	DefaultAvatarID string `json:"default_avatar_id"`
	IsAvatarEmpty   bool   `json:"is_avatar_empty"`
}

// GetAvatarURL returns the full avatar URL for Yandex user
func (y *YandexUserInfo) GetAvatarURL() string {
	if y.IsAvatarEmpty || y.DefaultAvatarID == "" {
		return ""
	}
	return fmt.Sprintf("https://avatars.yandex.net/get-yapic/%s/islands-200", y.DefaultAvatarID)
}

// getYandexRedirectURL returns the OAuth redirect URL based on domain
func (h *Handler) getYandexRedirectURL() string {
	if h.Domain == "localhost" || h.Domain == "127.0.0.1" {
		return fmt.Sprintf("http://%s/auth/yandex/callback", h.Domain)
	}
	return fmt.Sprintf("https://app.%s/auth/yandex/callback", h.Domain)
}

// generateState generates a random state parameter for OAuth
func generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

// YandexAuth initiates Yandex OAuth flow
func (h *Handler) YandexAuth(c *gin.Context) {
	if h.YandexClientID == "" {
		c.String(http.StatusNotFound, "Yandex OAuth not configured")
		return
	}

	state := generateState()

	// Store state in cookie for verification
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		Secure:   h.Domain != "localhost" && h.Domain != "127.0.0.1",
		SameSite: http.SameSiteLaxMode,
	})

	// Build authorization URL
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", h.YandexClientID)
	params.Set("redirect_uri", h.getYandexRedirectURL())
	params.Set("state", state)
	params.Set("scope", "login:email login:info login:avatar")

	authURL := "https://oauth.yandex.ru/authorize?" + params.Encode()
	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

// YandexCallback handles OAuth callback from Yandex
func (h *Handler) YandexCallback(c *gin.Context) {
	// Verify state
	stateCookie, err := c.Cookie("oauth_state")
	if err != nil {
		c.String(http.StatusBadRequest, "Missing state cookie")
		return
	}

	state := c.Query("state")
	if state == "" || state != stateCookie {
		c.String(http.StatusBadRequest, "Invalid state parameter")
		return
	}

	// Clear state cookie
	http.SetCookie(c.Writer, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Check for error
	if errMsg := c.Query("error"); errMsg != "" {
		log.Printf("Yandex OAuth error: %s - %s", errMsg, c.Query("error_description"))
		c.Redirect(http.StatusTemporaryRedirect, "/login")
		return
	}

	code := c.Query("code")
	if code == "" {
		c.String(http.StatusBadRequest, "Missing authorization code")
		return
	}

	// Exchange code for token
	tokenData := url.Values{}
	tokenData.Set("grant_type", "authorization_code")
	tokenData.Set("code", code)
	tokenData.Set("client_id", h.YandexClientID)
	tokenData.Set("client_secret", h.YandexClientSecret)

	tokenResp, err := http.PostForm("https://oauth.yandex.ru/token", tokenData)
	if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to exchange code for token")
		c.String(http.StatusInternalServerError, "Failed to authenticate with Yandex")
		return
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		sentry.CaptureErrorWithContext(c, fmt.Errorf("token exchange failed: %s", string(body)), "Yandex token exchange failed")
		c.String(http.StatusInternalServerError, "Failed to authenticate with Yandex")
		return
	}

	var tokenResult struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}

	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenResult); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to decode token response")
		c.String(http.StatusInternalServerError, "Failed to authenticate with Yandex")
		return
	}

	// Get user info
	userReq, _ := http.NewRequest("GET", "https://login.yandex.ru/info", nil)
	userReq.Header.Set("Authorization", "OAuth "+tokenResult.AccessToken)

	userResp, err := http.DefaultClient.Do(userReq)
	if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to get user info from Yandex")
		c.String(http.StatusInternalServerError, "Failed to get user info from Yandex")
		return
	}
	defer userResp.Body.Close()

	// Read raw response for debugging
	userBody, _ := io.ReadAll(userResp.Body)
	log.Printf("Yandex user info raw response: %s", string(userBody))

	var yandexUser YandexUserInfo
	if err := json.Unmarshal(userBody, &yandexUser); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to decode Yandex user info")
		c.String(http.StatusInternalServerError, "Failed to get user info from Yandex")
		return
	}

	log.Printf("Yandex user parsed: ID=%s, AvatarID=%s, IsAvatarEmpty=%v, AvatarURL=%s",
		yandexUser.ID, yandexUser.DefaultAvatarID, yandexUser.IsAvatarEmpty, yandexUser.GetAvatarURL())

	// Check if user is already logged in (linking account)
	if existingUser, err := h.getUserFromSession(c); err == nil {
		// User is logged in - link Yandex account to existing user
		if err := storage.LinkYandexAccount(existingUser.ID, yandexUser.ID); err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to link Yandex account")
			c.String(http.StatusInternalServerError, "Failed to link Yandex account")
			return
		}
		c.Redirect(http.StatusTemporaryRedirect, "/")
		return
	}

	// Try to find existing user by Yandex ID
	user, err := storage.GetUserByYandexID(yandexUser.ID)

	if err == storage.ErrNotFound {
		// Create new user with token and domains
		newUser := &models.User{
			YandexID:  &yandexUser.ID,
			Email:     yandexUser.DefaultEmail,
			FirstName: yandexUser.FirstName,
			LastName:  yandexUser.LastName,
			Username:  yandexUser.Login,
		}

		// Generate domain names
		prefixes := []string{"misty", "silent", "bold", "rapid", "cool"}
		suffixes := []string{"river", "star", "eagle", "bear", "fox"}
		var domains []string
		for i := 0; i < h.DomainsPerUser; i++ {
			name := fmt.Sprintf("%s-%s-%d", prefixes[i%len(prefixes)], suffixes[i%len(suffixes)], time.Now().Unix()%1000+int64(i))
			domains = append(domains, name)
		}

		reg := storage.UserRegistration{
			User:    newUser,
			Domains: domains,
		}

		createdUser, _, err := storage.CreateUserWithTokenAndDomains(reg)
		if err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to create user via Yandex OAuth")
			c.String(http.StatusInternalServerError, "Failed to create user account")
			return
		}
		user = createdUser
		if h.AppMetrics != nil {
			h.AppMetrics.UserCreated()
		}

		// Download and save avatar locally
		if avatarURL := yandexUser.GetAvatarURL(); avatarURL != "" {
			if localURL := avatar.Download(user.ID, avatarURL); localURL != "" {
				user.PhotoURL = localURL
				storage.UpdateUser(user)
			}
		}
	} else if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Database error looking up Yandex user")
		c.String(http.StatusInternalServerError, "Database error")
		return
	} else {
		// Update existing user info
		user.FirstName = yandexUser.FirstName
		user.LastName = yandexUser.LastName
		user.Username = yandexUser.Login
		if yandexUser.DefaultEmail != "" {
			user.Email = yandexUser.DefaultEmail
		}

		// Download and save avatar locally
		if avatarURL := yandexUser.GetAvatarURL(); avatarURL != "" {
			if localURL := avatar.Download(user.ID, avatarURL); localURL != "" {
				user.PhotoURL = localURL
			}
		}

		if err := storage.UpdateUser(user); err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to update Yandex user")
			c.String(http.StatusInternalServerError, "Failed to update user")
			return
		}
	}

	// Set session
	if err := h.Session.SetSession(c.Writer, user.ID); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to set session after Yandex login")
		c.String(http.StatusInternalServerError, "Failed to create session")
		return
	}

	c.Redirect(http.StatusTemporaryRedirect, "/")
}

// YandexTokenPage serves the auxiliary page that receives the token from Yandex SDK
func (h *Handler) YandexTokenPage(c *gin.Context) {
	c.HTML(http.StatusOK, "yandex_token.html", gin.H{})
}

// YandexTokenAuth handles authentication with Yandex access token from SDK
func (h *Handler) YandexTokenAuth(c *gin.Context) {
	var req struct {
		AccessToken string `json:"access_token"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	if req.AccessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing access token"})
		return
	}

	// Get user info from Yandex using the access token
	userReq, _ := http.NewRequest("GET", "https://login.yandex.ru/info", nil)
	userReq.Header.Set("Authorization", "OAuth "+req.AccessToken)

	userResp, err := http.DefaultClient.Do(userReq)
	if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to get user info from Yandex (SDK)")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user info from Yandex"})
		return
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid access token"})
		return
	}

	userBody, _ := io.ReadAll(userResp.Body)
	log.Printf("Yandex user info (SDK) raw response: %s", string(userBody))

	var yandexUser YandexUserInfo
	if err := json.Unmarshal(userBody, &yandexUser); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to decode Yandex user info (SDK)")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse user info"})
		return
	}

	log.Printf("Yandex user (SDK) parsed: ID=%s, AvatarID=%s, IsAvatarEmpty=%v",
		yandexUser.ID, yandexUser.DefaultAvatarID, yandexUser.IsAvatarEmpty)

	// Check if user is already logged in (linking account)
	if existingUser, err := h.getUserFromSession(c); err == nil {
		if err := storage.LinkYandexAccount(existingUser.ID, yandexUser.ID); err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to link Yandex account (SDK)")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to link Yandex account"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}

	// Try to find existing user by Yandex ID
	user, err := storage.GetUserByYandexID(yandexUser.ID)

	if err == storage.ErrNotFound {
		// Create new user with token and domains
		newUser := &models.User{
			YandexID:  &yandexUser.ID,
			Email:     yandexUser.DefaultEmail,
			FirstName: yandexUser.FirstName,
			LastName:  yandexUser.LastName,
			Username:  yandexUser.Login,
		}

		// Generate domain names
		prefixes := []string{"misty", "silent", "bold", "rapid", "cool"}
		suffixes := []string{"river", "star", "eagle", "bear", "fox"}
		var domains []string
		for i := 0; i < h.DomainsPerUser; i++ {
			name := fmt.Sprintf("%s-%s-%d", prefixes[i%len(prefixes)], suffixes[i%len(suffixes)], time.Now().Unix()%1000+int64(i))
			domains = append(domains, name)
		}

		reg := storage.UserRegistration{
			User:    newUser,
			Domains: domains,
		}

		createdUser, _, err := storage.CreateUserWithTokenAndDomains(reg)
		if err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to create user via Yandex SDK")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user account"})
			return
		}
		user = createdUser
		if h.AppMetrics != nil {
			h.AppMetrics.UserCreated()
		}

		// Download and save avatar locally
		if avatarURL := yandexUser.GetAvatarURL(); avatarURL != "" {
			if localURL := avatar.Download(user.ID, avatarURL); localURL != "" {
				user.PhotoURL = localURL
				storage.UpdateUser(user)
			}
		}
	} else if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Database error looking up Yandex user (SDK)")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	} else {
		// Update existing user info
		user.FirstName = yandexUser.FirstName
		user.LastName = yandexUser.LastName
		user.Username = yandexUser.Login
		if yandexUser.DefaultEmail != "" {
			user.Email = yandexUser.DefaultEmail
		}

		// Download and save avatar locally
		if avatarURL := yandexUser.GetAvatarURL(); avatarURL != "" {
			if localURL := avatar.Download(user.ID, avatarURL); localURL != "" {
				user.PhotoURL = localURL
			}
		}

		if err := storage.UpdateUser(user); err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to update Yandex user (SDK)")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update user"})
			return
		}
	}

	// Set session
	if err := h.Session.SetSession(c.Writer, user.ID); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to set session after Yandex SDK login")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// LinkTelegram initiates Telegram account linking for logged-in user
func (h *Handler) LinkTelegram(c *gin.Context) {
	user, err := h.getUserFromSession(c)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login")
		return
	}

	// If user already has Telegram linked, redirect to index
	if user.TelegramID != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/")
		return
	}

	var authURL string
	if h.Domain == "localhost" || h.Domain == "127.0.0.1" {
		authURL = fmt.Sprintf("http://%s/auth/telegram/link", h.Domain)
	} else {
		authURL = fmt.Sprintf("https://app.%s/auth/telegram/link", h.Domain)
	}

	c.HTML(http.StatusOK, "link_telegram.html", gin.H{
		"BotName":               h.BotName,
		"AuthURL":               authURL,
		"GitHubRepo":            h.GitHubRepo,
		"Version":               version.Version,
		"User":                  user,
		"TelegramBotEnabled":    h.BotName != "" && h.BotToken != "",
		"TelegramWidgetEnabled": h.TelegramWidgetEnabled,
	})
}

// TelegramLinkCallback handles Telegram OAuth callback for account linking
func (h *Handler) TelegramLinkCallback(c *gin.Context) {
	// Verify user is logged in
	user, err := h.getUserFromSession(c)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/login")
		return
	}

	// Verify Telegram hash
	if !h.verifyTelegramHash(c.Request.URL.Query()) {
		c.String(http.StatusUnauthorized, "Invalid Telegram Hash")
		return
	}

	data := c.Request.URL.Query()
	idStr := data.Get("id")
	var tgID int64
	fmt.Sscanf(idStr, "%d", &tgID)

	// Check if this Telegram ID is already linked to another account
	existingUser, err := storage.GetUserByTelegramID(tgID)
	if err == nil && existingUser.ID != user.ID {
		c.String(http.StatusConflict, "This Telegram account is already linked to another user")
		return
	}

	// Link Telegram to current user
	if err := storage.LinkTelegramAccount(user.ID, tgID); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to link Telegram account")
		c.String(http.StatusInternalServerError, "Failed to link Telegram account")
		return
	}

	// Update user info from Telegram
	user.TelegramID = &tgID
	user.FirstName = data.Get("first_name")
	user.LastName = data.Get("last_name")
	if username := data.Get("username"); username != "" {
		user.Username = username
	}

	// Download and save avatar locally
	if photoURL := data.Get("photo_url"); photoURL != "" {
		if localURL := avatar.Download(user.ID, photoURL); localURL != "" {
			user.PhotoURL = localURL
		}
	}

	if err := storage.UpdateUser(user); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to update user after Telegram link")
	}

	c.Redirect(http.StatusTemporaryRedirect, "/")
}

// InitTelegramAuth generates a new auth hash for Telegram bot authentication
// GET /api/telegram-auth/init
func (h *Handler) InitTelegramAuth(c *gin.Context) {
	if h.TelegramBot == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram auth not configured"})
		return
	}

	ip := c.ClientIP()
	userAgent := c.Request.UserAgent()

	// Check if user is already logged in (linking mode)
	isLinking := false
	var userID uint
	if user, err := h.getUserFromSession(c); err == nil {
		isLinking = true
		userID = user.ID
	}

	hash, err := h.TelegramBot.GetPendingLogins().Create(ip, userAgent, isLinking, userID)
	if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to create pending login")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate auth link"})
		return
	}

	deepLink := fmt.Sprintf("https://t.me/%s?start=%s", h.TelegramBot.GetBotName(), hash)

	c.JSON(http.StatusOK, gin.H{
		"hash":      hash,
		"deep_link": deepLink,
		"ttl":       300, // 5 minutes in seconds
	})
}

// PollTelegramAuth checks the status of a pending login
// GET /api/telegram-auth/poll?hash=xxx
func (h *Handler) PollTelegramAuth(c *gin.Context) {
	hash := c.Query("hash")
	if hash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing hash"})
		return
	}

	if h.TelegramBot == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram auth not configured"})
		return
	}

	pending, ok := h.TelegramBot.GetPendingLogins().Get(hash)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}

	switch pending.Status {
	case telegram.StatusPending:
		c.JSON(http.StatusOK, gin.H{"status": "pending"})

	case telegram.StatusRejected:
		h.TelegramBot.GetPendingLogins().Delete(hash)
		c.JSON(http.StatusOK, gin.H{"status": "rejected"})

	case telegram.StatusApproved:
		login, ok := h.TelegramBot.GetPendingLogins().Consume(hash)
		if !ok {
			c.JSON(http.StatusOK, gin.H{"status": "expired"})
			return
		}

		if login.IsLinking {
			if err := h.handleTelegramLinkFromBot(c, login); err != nil {
				c.JSON(http.StatusConflict, gin.H{
					"status": "error",
					"error":  err.Error(),
				})
				return
			}
		} else {
			if err := h.handleTelegramLoginFromBot(c, login); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"status": "error",
					"error":  err.Error(),
				})
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{"status": "approved"})
	}
}

// handleTelegramLoginFromBot creates/updates user and sets session
func (h *Handler) handleTelegramLoginFromBot(c *gin.Context, login *telegram.PendingLogin) error {
	user, err := storage.GetUserByTelegramID(login.TelegramID)

	if err == storage.ErrNotFound {
		// Create new user (without photo URL - we'll download it after)
		newUser := &models.User{
			TelegramID: &login.TelegramID,
			FirstName:  login.FirstName,
			LastName:   login.LastName,
			Username:   login.Username,
		}

		// Generate domains
		prefixes := []string{"misty", "silent", "bold", "rapid", "cool"}
		suffixes := []string{"river", "star", "eagle", "bear", "fox"}
		var domains []string
		for i := 0; i < h.DomainsPerUser; i++ {
			name := fmt.Sprintf("%s-%s-%d", prefixes[i%len(prefixes)], suffixes[i%len(suffixes)], time.Now().Unix()%1000+int64(i))
			domains = append(domains, name)
		}

		reg := storage.UserRegistration{
			User:    newUser,
			Domains: domains,
		}

		createdUser, _, err := storage.CreateUserWithTokenAndDomains(reg)
		if err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to create user via Telegram bot auth")
			return err
		}
		user = createdUser
		if h.AppMetrics != nil {
			h.AppMetrics.UserCreated()
		}

		// Download and save avatar locally (using DB user ID)
		if login.PhotoURL != "" {
			if localURL := avatar.Download(user.ID, login.PhotoURL); localURL != "" {
				user.PhotoURL = localURL
				storage.UpdateUser(user)
			}
		}
	} else if err != nil {
		sentry.CaptureErrorWithContext(c, err, "Database error looking up user")
		return err
	} else {
		// Update existing user info
		user.FirstName = login.FirstName
		user.LastName = login.LastName
		user.Username = login.Username

		// Download new avatar if provided
		if login.PhotoURL != "" {
			if localURL := avatar.Download(user.ID, login.PhotoURL); localURL != "" {
				user.PhotoURL = localURL
			}
		}

		if err := storage.UpdateUser(user); err != nil {
			sentry.CaptureErrorWithContext(c, err, "Failed to update user")
			return err
		}
	}

	return h.Session.SetSession(c.Writer, user.ID)
}

// handleTelegramLinkFromBot links Telegram to existing account
func (h *Handler) handleTelegramLinkFromBot(c *gin.Context, login *telegram.PendingLogin) error {
	// Check if this Telegram is already linked to another account
	existingUser, err := storage.GetUserByTelegramID(login.TelegramID)
	if err == nil && existingUser.ID != login.UserID {
		return fmt.Errorf("этот Telegram аккаунт уже привязан к другому пользователю")
	}

	if err := storage.LinkTelegramAccount(login.UserID, login.TelegramID); err != nil {
		sentry.CaptureErrorWithContext(c, err, "Failed to link Telegram account")
		return err
	}

	user, err := storage.GetUserByID(login.UserID)
	if err != nil {
		return err
	}

	user.TelegramID = &login.TelegramID
	user.FirstName = login.FirstName
	user.LastName = login.LastName
	if login.Username != "" {
		user.Username = login.Username
	}

	// Download and save avatar locally
	if login.PhotoURL != "" {
		if localURL := avatar.Download(user.ID, login.PhotoURL); localURL != "" {
			user.PhotoURL = localURL
		}
	}

	return storage.UpdateUser(user)
}

// ServeAvatar serves a user's avatar image from local storage.
// URL format: /avatars/{user_id}.jpg
func (h *Handler) ServeAvatar(c *gin.Context) {
	// Extract filename from path (e.g., "123.jpg")
	path := c.Request.URL.Path
	filename := filepath.Base(path)

	// Validate filename format
	if !strings.HasSuffix(filename, ".jpg") {
		c.String(http.StatusNotFound, "Not Found")
		return
	}

	// Build full path
	filePath := filepath.Join(avatar.AvatarDir, filename)

	// Security: ensure we're not serving files outside avatar directory
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		c.String(http.StatusNotFound, "Not Found")
		return
	}
	absAvatarDir, _ := filepath.Abs(avatar.AvatarDir)
	if !strings.HasPrefix(absPath, absAvatarDir) {
		c.String(http.StatusForbidden, "Forbidden")
		return
	}

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.String(http.StatusNotFound, "Not Found")
		return
	}

	// Serve the file with caching headers
	c.Header("Cache-Control", "public, max-age=86400") // 24 hours
	c.File(filePath)
}

// Metrics serves the Prometheus metrics endpoint.
func (h *Handler) Metrics(c *gin.Context) {
	if h.AppMetrics == nil {
		c.String(http.StatusNotFound, "Metrics not configured")
		return
	}

	// Check Bearer token if configured
	if h.MetricsToken != "" {
		auth := c.GetHeader("Authorization")
		if auth != "Bearer "+h.MetricsToken {
			c.String(http.StatusUnauthorized, "Unauthorized")
			return
		}
	}

	h.AppMetrics.Handler()(c)
}
