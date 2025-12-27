package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gopublic/internal/storage"
)

// Bot handles Telegram bot interactions for admin statistics
type Bot struct {
	token         string
	adminID       int64
	stopCh        chan struct{}
	lastUpdateID  int64
}

// NewBot creates a new Telegram bot instance
func NewBot(token string, adminID int64) *Bot {
	return &Bot{
		token:   token,
		adminID: adminID,
		stopCh:  make(chan struct{}),
	}
}

// Start begins the long polling loop for receiving updates
func (b *Bot) Start() {
	if b.token == "" || b.adminID == 0 {
		log.Println("Telegram bot not configured (missing token or admin ID)")
		return
	}

	log.Println("Starting Telegram admin bot...")

	go b.pollUpdates()
}

// Stop gracefully stops the bot
func (b *Bot) Stop() {
	close(b.stopCh)
}

// Update represents a Telegram update
type Update struct {
	UpdateID int64   `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message represents a Telegram message
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      *Chat  `json:"chat"`
	Text      string `json:"text,omitempty"`
}

// User represents a Telegram user
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat represents a Telegram chat
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// GetUpdatesResponse represents the response from getUpdates
type GetUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

func (b *Bot) pollUpdates() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopCh:
			return
		case <-ticker.C:
			updates, err := b.getUpdates()
			if err != nil {
				log.Printf("Error getting updates: %v", err)
				continue
			}

			for _, update := range updates {
				b.handleUpdate(update)
				b.lastUpdateID = update.UpdateID
			}
		}
	}
}

func (b *Bot) getUpdates() ([]Update, error) {
	params := url.Values{}
	params.Set("offset", fmt.Sprintf("%d", b.lastUpdateID+1))
	params.Set("timeout", "30")
	params.Set("allowed_updates", `["message"]`)

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?%s", b.token, params.Encode())

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response GetUpdatesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	if !response.OK {
		return nil, fmt.Errorf("telegram API returned not OK")
	}

	return response.Result, nil
}

func (b *Bot) handleUpdate(update Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message

	// Only respond to admin (check both user ID and chat ID for security)
	if msg.From == nil || msg.From.ID != b.adminID {
		// Silently ignore messages from non-admins
		return
	}

	// Also verify the chat is a private chat with admin (not a group)
	if msg.Chat.ID != b.adminID {
		return
	}

	// Handle commands
	text := strings.TrimSpace(msg.Text)
	switch {
	case text == "/stats" || text == "/start":
		b.sendStats(msg.Chat.ID)
	case text == "/help":
		b.sendHelp(msg.Chat.ID)
	}
}

func (b *Bot) sendStats(chatID int64) {
	// Get total users
	userCount, err := storage.GetTotalUserCount()
	if err != nil {
		b.sendMessage(chatID, fmt.Sprintf("❌ Ошибка получения статистики: %v", err))
		return
	}

	// Get top users today
	topToday, err := storage.GetTopUsersByBandwidthToday(10)
	if err != nil {
		log.Printf("Error getting top users today: %v", err)
	}

	// Get top users all time
	topAllTime, err := storage.GetTopUsersByBandwidthAllTime(10)
	if err != nil {
		log.Printf("Error getting top users all time: %v", err)
	}

	// Build message
	var sb strings.Builder

	sb.WriteString("📊 *Статистика GoPublic*\n\n")
	sb.WriteString(fmt.Sprintf("👥 *Всего пользователей:* %d\n\n", userCount))

	// Top today
	sb.WriteString("📈 *ТОП-10 за сегодня:*\n")
	if len(topToday) == 0 {
		sb.WriteString("_Нет активности за сегодня_\n")
	} else {
		for i, u := range topToday {
			sb.WriteString(fmt.Sprintf("%d. %s — %s\n", i+1, formatUserInfo(u), formatBytes(u.BytesUsed)))
		}
	}

	sb.WriteString("\n")

	// Top all time
	sb.WriteString("🏆 *ТОП-10 за всё время:*\n")
	if len(topAllTime) == 0 {
		sb.WriteString("_Нет данных_\n")
	} else {
		for i, u := range topAllTime {
			sb.WriteString(fmt.Sprintf("%d. %s — %s\n", i+1, formatUserInfo(u), formatBytes(u.BytesUsed)))
		}
	}

	b.sendMessage(chatID, sb.String())
}

func (b *Bot) sendHelp(chatID int64) {
	help := `🤖 *Команды бота:*

/stats — Показать статистику
/help — Показать справку

Бот показывает статистику только администратору.`

	b.sendMessage(chatID, help)
}

func (b *Bot) sendMessage(chatID int64, text string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)

	params := url.Values{}
	params.Set("chat_id", fmt.Sprintf("%d", chatID))
	params.Set("text", text)
	params.Set("parse_mode", "Markdown")

	resp, err := http.PostForm(apiURL, params)
	if err != nil {
		log.Printf("Error sending message: %v", err)
		return
	}
	defer resp.Body.Close()
}

// formatUserInfo formats user information for display
func formatUserInfo(u storage.UserStats) string {
	var parts []string

	// Name
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		parts = append(parts, name)
	}

	// Username (Telegram or Yandex)
	if u.Username != "" {
		parts = append(parts, fmt.Sprintf("@%s", u.Username))
	}

	// Email
	if u.Email != "" {
		parts = append(parts, u.Email)
	}

	// Identifiers
	if u.TelegramID != nil {
		parts = append(parts, fmt.Sprintf("TG:%d", *u.TelegramID))
	} else if u.YandexID != nil {
		parts = append(parts, fmt.Sprintf("Ya:%s", *u.YandexID))
	}

	if len(parts) == 0 {
		return fmt.Sprintf("User#%d", u.UserID)
	}

	return strings.Join(parts, " | ")
}

// formatBytes formats bytes into human-readable format
func formatBytes(bytes int64) string {
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
}
