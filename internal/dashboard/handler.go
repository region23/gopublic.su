package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gopublic/internal/auth"
	"gopublic/internal/models"
	"gopublic/internal/storage"
)

//go:embed templates/*
var templateFS embed.FS

type Handler struct {
	BotToken string
	BotName  string
	Domain   string
	Session  *auth.SessionManager
}

func NewHandler() (*Handler, error) {
	domain := os.Getenv("DOMAIN_NAME")
	isSecure := domain != "" && domain != "localhost" && domain != "127.0.0.1"

	// In dev mode (insecure), allow random session keys
	// In production (secure), require proper session keys
	sessionCfg := auth.SessionConfig{
		IsSecure:          isSecure,
		AllowInsecureKeys: !isSecure, // Only allow random keys in dev mode
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
	// Parse templates
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
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
		"BotName": h.BotName,
		"AuthURL": authURL,
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
		log.Printf("Failed to fetch token for user %d: %v", user.ID, err)
		c.String(http.StatusInternalServerError, "Failed to load user data")
		return
	}

	// Fetch domains
	domains, err := storage.GetUserDomains(user.ID)
	if err != nil {
		log.Printf("Failed to fetch domains for user %d: %v", user.ID, err)
		c.String(http.StatusInternalServerError, "Failed to load user data")
		return
	}

	c.HTML(http.StatusOK, "index.html", gin.H{
		"User":       user,
		"Token":      token.TokenString,
		"Domains":    domains,
		"RootDomain": h.Domain,
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

	// Find or Create User
	user, err := storage.GetUserByTelegramID(tgID)

	if err == storage.ErrNotFound {
		// Create new user with token and domains in a single transaction
		newUser := &models.User{
			TelegramID: tgID,
			FirstName:  firstName,
			LastName:   lastName,
			Username:   username,
			PhotoURL:   photoURL,
		}

		// Generate domain names
		prefixes := []string{"misty", "silent", "bold", "rapid", "cool"}
		suffixes := []string{"river", "star", "eagle", "bear", "fox"}
		var domains []string
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("%s-%s-%d", prefixes[i%len(prefixes)], suffixes[i%len(suffixes)], time.Now().Unix()%1000+int64(i))
			domains = append(domains, name)
		}

		reg := storage.UserRegistration{
			User:    newUser,
			Domains: domains,
		}

		createdUser, _, err := storage.CreateUserWithTokenAndDomains(reg)
		if err != nil {
			log.Printf("Failed to create user: %v", err)
			c.String(http.StatusInternalServerError, "Failed to create user account")
			return
		}
		user = createdUser
	} else if err != nil {
		log.Printf("Database error looking up user: %v", err)
		c.String(http.StatusInternalServerError, "Database error")
		return
	} else {
		// Update existing user info
		user.FirstName = firstName
		user.LastName = lastName
		user.Username = username
		user.PhotoURL = photoURL
		if err := storage.UpdateUser(user); err != nil {
			log.Printf("Failed to update user: %v", err)
			c.String(http.StatusInternalServerError, "Failed to update user")
			return
		}
	}

	// Set secure signed session cookie
	if err := h.Session.SetSession(c.Writer, user.ID); err != nil {
		log.Printf("Failed to set session: %v", err)
		c.String(http.StatusInternalServerError, "Failed to create session")
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, "/")
}

func (h *Handler) Logout(c *gin.Context) {
	h.Session.ClearSession(c.Writer)
	c.Redirect(http.StatusTemporaryRedirect, "/login")
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
