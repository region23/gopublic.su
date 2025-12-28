package telegram

import (
	"context"
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
	token        string
	adminID      int64
	stopCh       chan struct{}
	lastUpdateID int64
	client       *http.Client
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewBot creates a new Telegram bot instance
func NewBot(token string, adminID int64) *Bot {
	return &Bot{
		token:   token,
		adminID: adminID,
		stopCh:  make(chan struct{}),
		client: &http.Client{
			Timeout: 35 * time.Second, // Slightly longer than Telegram's 30s long-polling timeout
		},
	}
}

// Start begins the long polling loop for receiving updates
func (b *Bot) Start() {
	if b.token == "" || b.adminID == 0 {
		log.Println("Telegram bot not configured (missing token or admin ID)")
		return
	}

	log.Println("Starting Telegram admin bot...")

	b.ctx, b.cancel = context.WithCancel(context.Background())
	go b.pollUpdates()
}

// Stop gracefully stops the bot
func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel() // Cancel context to interrupt any in-flight HTTP requests
	}
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
	log.Println("Telegram bot: starting poll loop")

	// Make first request immediately, then use ticker
	for {
		updates, err := b.getUpdates()
		if err != nil {
			// Don't log context cancellation errors during shutdown
			if b.ctx.Err() != nil {
				log.Println("Telegram bot: context cancelled, stopping")
				return
			}
			log.Printf("Telegram bot: error getting updates: %v", err)
			// Wait before retry on error
			select {
			case <-b.stopCh:
				return
			case <-b.ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		for _, update := range updates {
			b.handleUpdate(update)
			b.lastUpdateID = update.UpdateID
		}

		// Check for stop signal between requests
		select {
		case <-b.stopCh:
			log.Println("Telegram bot: received stop signal")
			return
		case <-b.ctx.Done():
			log.Println("Telegram bot: context done")
			return
		default:
			// Continue immediately to next long-poll request
		}
	}
}

func (b *Bot) getUpdates() ([]Update, error) {
	params := url.Values{}
	params.Set("offset", fmt.Sprintf("%d", b.lastUpdateID+1))
	params.Set("timeout", "30")
	params.Set("allowed_updates", `["message"]`)

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?%s", b.token, params.Encode())

	log.Printf("Telegram bot: requesting updates (offset=%d)...", b.lastUpdateID+1)

	req, err := http.NewRequestWithContext(b.ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("Telegram bot: got response status %d", resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var response GetUpdatesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("unmarshal: %w (body: %s)", err, string(body)[:min(200, len(body))])
	}

	if !response.OK {
		return nil, fmt.Errorf("telegram API returned not OK (body: %s)", string(body)[:min(200, len(body))])
	}

	log.Printf("Telegram bot: got %d updates", len(response.Result))
	return response.Result, nil
}

func (b *Bot) handleUpdate(update Update) {
	if update.Message == nil {
		log.Printf("Telegram bot: update %d has no message", update.UpdateID)
		return
	}

	msg := update.Message
	log.Printf("Telegram bot: message from user %d (admin=%d), chat %d, text: %s",
		msg.From.ID, b.adminID, msg.Chat.ID, msg.Text)

	// Only respond to admin (check both user ID and chat ID for security)
	if msg.From == nil || msg.From.ID != b.adminID {
		log.Printf("Telegram bot: ignoring message from non-admin user %d", msg.From.ID)
		return
	}

	// Also verify the chat is a private chat with admin (not a group)
	if msg.Chat.ID != b.adminID {
		log.Printf("Telegram bot: ignoring message from non-admin chat %d", msg.Chat.ID)
		return
	}

	// Handle commands
	text := strings.TrimSpace(msg.Text)
	log.Printf("Telegram bot: processing command: %s", text)
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

	log.Printf("Telegram bot: sending message to %d (len=%d)", chatID, len(text))

	req, err := http.NewRequestWithContext(b.ctx, "POST", apiURL, strings.NewReader(params.Encode()))
	if err != nil {
		log.Printf("Telegram bot: error creating request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.client.Do(req)
	if err != nil {
		if b.ctx.Err() == nil {
			log.Printf("Telegram bot: error sending message: %v", err)
		}
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	log.Printf("Telegram bot: sendMessage response: status=%d, body=%s", resp.StatusCode, string(body)[:min(200, len(body))])
}

// escapeMarkdown escapes special Markdown characters
func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"`", "\\`",
	)
	return replacer.Replace(s)
}

// formatUserInfo formats user information for display
func formatUserInfo(u storage.UserStats) string {
	var parts []string

	// Name
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		parts = append(parts, escapeMarkdown(name))
	}

	// Username (Telegram or Yandex)
	if u.Username != "" {
		parts = append(parts, fmt.Sprintf("@%s", escapeMarkdown(u.Username)))
	}

	// Email
	if u.Email != "" {
		parts = append(parts, escapeMarkdown(u.Email))
	}

	// Identifiers
	if u.TelegramID != nil {
		parts = append(parts, fmt.Sprintf("TG:%d", *u.TelegramID))
	} else if u.YandexID != nil {
		parts = append(parts, fmt.Sprintf("Ya:%s", escapeMarkdown(*u.YandexID)))
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
