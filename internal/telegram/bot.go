package telegram

import (
	"bytes"
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

// Bot handles Telegram bot interactions for admin statistics and auth
type Bot struct {
	token         string
	botName       string
	adminID       int64
	stopCh        chan struct{}
	lastUpdateID  int64
	pendingLogins *PendingLoginStore
	client        *http.Client
	ctx           context.Context
	cancel        context.CancelFunc
}

// NewBot creates a new Telegram bot instance
func NewBot(token string, botName string, adminID int64) *Bot {
	return &Bot{
		token:         token,
		botName:       botName,
		adminID:       adminID,
		stopCh:        make(chan struct{}),
		pendingLogins: NewPendingLoginStore(),
		client: &http.Client{
			Timeout: 35 * time.Second, // Slightly longer than Telegram's 30s long-polling timeout
		},
	}
}

// GetPendingLogins returns the pending login store
func (b *Bot) GetPendingLogins() *PendingLoginStore {
	return b.pendingLogins
}

// GetBotName returns the bot username for deep links
func (b *Bot) GetBotName() string {
	return b.botName
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
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
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
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Chat represents a Telegram chat
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// CallbackQuery represents a callback query from inline keyboard
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// InlineKeyboardButton represents an inline keyboard button
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// InlineKeyboardMarkup represents an inline keyboard
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// GetUpdatesResponse represents the response from getUpdates
type GetUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// GetUserProfilePhotosResponse represents the response from getUserProfilePhotos
type GetUserProfilePhotosResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		TotalCount int `json:"total_count"`
		Photos     [][]struct {
			FileID string `json:"file_id"`
		} `json:"photos"`
	} `json:"result"`
}

// GetFileResponse represents the response from getFile
type GetFileResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		FilePath string `json:"file_path"`
	} `json:"result"`
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
	params.Set("allowed_updates", `["message","callback_query"]`)

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
	// Handle callback queries (inline button presses)
	if update.CallbackQuery != nil {
		b.handleCallbackQuery(update.CallbackQuery)
		return
	}

	if update.Message == nil {
		log.Printf("Telegram bot: update %d has no message", update.UpdateID)
		return
	}

	msg := update.Message
	text := strings.TrimSpace(msg.Text)

	log.Printf("Telegram bot: message from user %d (admin=%d), chat %d, text: %s",
		msg.From.ID, b.adminID, msg.Chat.ID, text)

	// Handle /start with auth hash (any user can do this)
	if strings.HasPrefix(text, "/start ") {
		hash := strings.TrimPrefix(text, "/start ")
		b.handleAuthStart(msg, hash)
		return
	}

	// Admin commands - check permissions
	if msg.From == nil || msg.From.ID != b.adminID {
		log.Printf("Telegram bot: ignoring message from non-admin user %d", msg.From.ID)
		return
	}
	if msg.Chat.ID != b.adminID {
		log.Printf("Telegram bot: ignoring message from non-admin chat %d", msg.Chat.ID)
		return
	}

	log.Printf("Telegram bot: processing command: %s", text)
	switch {
	case text == "/stats":
		b.sendStats(msg.Chat.ID)
	case text == "/start":
		b.sendMessage(msg.Chat.ID, "Привет! Я бот GoPublic.\n\nИспользуйте /stats для просмотра статистики.")
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

// escapeMarkdown escapes special Markdown characters (for parse_mode=Markdown)
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

// GeoIPResponse represents the response from ip-api.com
type GeoIPResponse struct {
	Status      string `json:"status"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	Region      string `json:"region"`
	RegionName  string `json:"regionName"`
	City        string `json:"city"`
}

// getGeoLocation fetches location info for an IP address using ip-api.com
// Returns empty string if lookup fails (non-blocking, best-effort)
// Free tier: 45 requests/minute, no API key needed
func getGeoLocation(ip string) string {
	// Skip private/local IPs
	if ip == "" || ip == "127.0.0.1" || ip == "::1" ||
		strings.HasPrefix(ip, "192.168.") ||
		strings.HasPrefix(ip, "10.") ||
		strings.HasPrefix(ip, "172.16.") ||
		strings.HasPrefix(ip, "172.17.") ||
		strings.HasPrefix(ip, "172.18.") ||
		strings.HasPrefix(ip, "172.19.") ||
		strings.HasPrefix(ip, "172.2") || // 172.20-172.31
		strings.HasPrefix(ip, "172.3") {
		return ""
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://ip-api.com/json/%s?fields=status,country,countryCode,regionName,city", ip))
	if err != nil {
		// Network error, timeout, etc. - silently continue without geo
		return ""
	}
	defer resp.Body.Close()

	// Handle rate limiting (429) or other HTTP errors
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			log.Printf("GeoIP rate limited (429), skipping lookup")
		}
		return ""
	}

	var geo GeoIPResponse
	if err := json.NewDecoder(resp.Body).Decode(&geo); err != nil || geo.Status != "success" {
		return ""
	}

	// Format: "City, Region, Country" or "City, Country" or just "Country"
	parts := []string{}
	if geo.City != "" {
		parts = append(parts, geo.City)
	}
	if geo.RegionName != "" && geo.RegionName != geo.City {
		parts = append(parts, geo.RegionName)
	}
	if geo.Country != "" {
		parts = append(parts, geo.Country)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// escapeMarkdownV2 escapes special characters for MarkdownV2 parse mode
func escapeMarkdownV2(s string) string {
	// MarkdownV2 requires escaping: _ * [ ] ( ) ~ ` > # + - = | { } . !
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(s)
}

// handleAuthStart handles /start command with auth hash
func (b *Bot) handleAuthStart(msg *Message, hash string) {
	log.Printf("Telegram bot: handleAuthStart called with hash=%s from user=%d", hash, msg.From.ID)

	if msg.From == nil {
		log.Printf("Telegram bot: handleAuthStart - msg.From is nil")
		return
	}

	pending, ok := b.pendingLogins.Get(hash)
	if !ok {
		log.Printf("Telegram bot: handleAuthStart - hash not found in pending logins")
		b.sendMessage(msg.Chat.ID, "Ссылка для входа недействительна или истекла.")
		return
	}

	log.Printf("Telegram bot: handleAuthStart - found pending login, IP=%s, isLinking=%v", pending.IP, pending.IsLinking)

	browserInfo := parseUserAgent(pending.UserAgent)

	// Fetch and cache geo location if not already done
	if pending.GeoLocation == "" {
		pending.GeoLocation = getGeoLocation(pending.IP)
	}

	var actionText string
	if pending.IsLinking {
		actionText = "Привязать Telegram к аккаунту"
	} else {
		actionText = "Войти в аккаунт"
	}

	// Build IP line with optional geo location
	ipLine := fmt.Sprintf("IP-адрес: `%s`", pending.IP)
	if pending.GeoLocation != "" {
		ipLine += fmt.Sprintf(" (%s)", pending.GeoLocation)
	}

	text := fmt.Sprintf(
		"*%s GoPublic*\n\n"+
			"%s\n"+
			"Браузер: %s\n\n"+
			"Если это не вы — нажмите Отклонить.",
		actionText,
		ipLine,
		browserInfo,
	)

	// Telegram limits callback_data to 64 bytes
	// Using short prefixes: "a:" for approve, "r:" for reject
	keyboard := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "✓ Разрешить", CallbackData: "a:" + hash},
				{Text: "✗ Отклонить", CallbackData: "r:" + hash},
			},
		},
	}

	b.sendMessageWithKeyboard(msg.Chat.ID, text, keyboard)
}

// parseUserAgent extracts browser/OS info from User-Agent
func parseUserAgent(ua string) string {
	switch {
	case strings.Contains(ua, "Chrome") && !strings.Contains(ua, "Edg"):
		if strings.Contains(ua, "Windows") {
			return "Chrome (Windows)"
		} else if strings.Contains(ua, "Mac") {
			return "Chrome (macOS)"
		} else if strings.Contains(ua, "Linux") {
			return "Chrome (Linux)"
		} else if strings.Contains(ua, "Android") {
			return "Chrome (Android)"
		}
		return "Chrome"
	case strings.Contains(ua, "Firefox"):
		if strings.Contains(ua, "Windows") {
			return "Firefox (Windows)"
		} else if strings.Contains(ua, "Mac") {
			return "Firefox (macOS)"
		} else if strings.Contains(ua, "Linux") {
			return "Firefox (Linux)"
		}
		return "Firefox"
	case strings.Contains(ua, "Safari") && !strings.Contains(ua, "Chrome"):
		if strings.Contains(ua, "iPhone") {
			return "Safari (iPhone)"
		} else if strings.Contains(ua, "iPad") {
			return "Safari (iPad)"
		}
		return "Safari (macOS)"
	case strings.Contains(ua, "Edg"):
		return "Edge"
	default:
		if len(ua) > 50 {
			return ua[:50] + "..."
		}
		if ua == "" {
			return "Неизвестный браузер"
		}
		return ua
	}
}

func (b *Bot) sendMessageWithKeyboard(chatID int64, text string, keyboard *InlineKeyboardMarkup) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)

	payload := map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "Markdown",
		"reply_markup": keyboard,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Telegram bot: failed to marshal message request: %v", err)
		return
	}

	log.Printf("Telegram bot: sending auth message to chat %d", chatID)

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Telegram bot: error sending message with keyboard: %v", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	log.Printf("Telegram bot: sendMessageWithKeyboard response: status=%d, body=%s", resp.StatusCode, string(body))
}

// handleCallbackQuery handles inline keyboard button presses
func (b *Bot) handleCallbackQuery(cq *CallbackQuery) {
	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) != 2 {
		b.answerCallbackQuery(cq.ID, "Ошибка обработки", true)
		return
	}

	action, hash := parts[0], parts[1]

	switch action {
	case "a": // approve
		b.handleAuthApprove(cq, hash)
	case "r": // reject
		b.handleAuthReject(cq, hash)
	default:
		b.answerCallbackQuery(cq.ID, "Неизвестное действие", true)
	}
}

func (b *Bot) handleAuthApprove(cq *CallbackQuery, hash string) {
	// Get pending login info before approving (for history message)
	pending, ok := b.pendingLogins.Get(hash)
	if !ok {
		b.answerCallbackQuery(cq.ID, "Ссылка истекла или уже использована", true)
		if cq.Message != nil {
			b.editMessageText(cq.Message.Chat.ID, cq.Message.MessageID, "Ссылка для входа истекла.")
		}
		return
	}

	// Save info before approve modifies/deletes it
	ip := pending.IP
	geoLocation := pending.GeoLocation
	browserInfo := parseUserAgent(pending.UserAgent)

	photoURL := b.getUserAvatarURL(cq.From.ID)

	if !b.pendingLogins.Approve(hash, cq.From.ID, cq.From.FirstName, cq.From.LastName, cq.From.Username, photoURL) {
		b.answerCallbackQuery(cq.ID, "Ссылка истекла или уже использована", true)
		if cq.Message != nil {
			b.editMessageText(cq.Message.Chat.ID, cq.Message.MessageID, "Ссылка для входа истекла\\.")
		}
		return
	}

	b.answerCallbackQuery(cq.ID, "Вход разрешён!", false)
	if cq.Message != nil {
		// Build IP line with geo location
		ipLine := ip
		if geoLocation != "" {
			ipLine = fmt.Sprintf("%s \\(%s\\)", escapeMarkdownV2(ip), escapeMarkdownV2(geoLocation))
		} else {
			ipLine = escapeMarkdownV2(ip)
		}

		msg := fmt.Sprintf(
			"*✓ Вход разрешён*\n\n"+
				">IP: %s\n"+
				">Браузер: %s",
			ipLine, escapeMarkdownV2(browserInfo),
		)
		b.editMessageText(cq.Message.Chat.ID, cq.Message.MessageID, msg)
	}
}

func (b *Bot) handleAuthReject(cq *CallbackQuery, hash string) {
	// Get pending login info before rejecting (for history message)
	pending, ok := b.pendingLogins.Get(hash)
	var ip, geoLocation, browserInfo string
	if ok {
		ip = pending.IP
		geoLocation = pending.GeoLocation
		browserInfo = parseUserAgent(pending.UserAgent)
	}

	b.pendingLogins.Reject(hash)

	b.answerCallbackQuery(cq.ID, "Вход отклонён", false)
	if cq.Message != nil {
		var msg string
		if ip != "" {
			// Build IP line with geo location
			ipLine := escapeMarkdownV2(ip)
			if geoLocation != "" {
				ipLine = fmt.Sprintf("%s \\(%s\\)", escapeMarkdownV2(ip), escapeMarkdownV2(geoLocation))
			}

			msg = fmt.Sprintf(
				"*✗ Вход отклонён*\n\n"+
					">IP: %s\n"+
					">Браузер: %s\n\n"+
					"_Если это была попытка фишинга — будьте осторожны_",
				ipLine, escapeMarkdownV2(browserInfo),
			)
		} else {
			msg = "*✗ Вход отклонён*"
		}
		b.editMessageText(cq.Message.Chat.ID, cq.Message.MessageID, msg)
	}
}

func (b *Bot) answerCallbackQuery(queryID, text string, showAlert bool) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", b.token)

	payload := map[string]interface{}{
		"callback_query_id": queryID,
		"text":              text,
		"show_alert":        showAlert,
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Error answering callback query: %v", err)
		return
	}
	defer resp.Body.Close()
}

func (b *Bot) editMessageText(chatID int64, messageID int64, text string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", b.token)

	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "MarkdownV2",
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Error editing message: %v", err)
		return
	}
	defer resp.Body.Close()
}

// getUserAvatarURL gets the user's profile photo URL
func (b *Bot) getUserAvatarURL(userID int64) string {
	photosURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUserProfilePhotos?user_id=%d&limit=1", b.token, userID)
	resp, err := http.Get(photosURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var photosResp GetUserProfilePhotosResponse
	if err := json.NewDecoder(resp.Body).Decode(&photosResp); err != nil || !photosResp.OK {
		return ""
	}

	if photosResp.Result.TotalCount == 0 || len(photosResp.Result.Photos) == 0 {
		return ""
	}

	photos := photosResp.Result.Photos[0]
	if len(photos) == 0 {
		return ""
	}
	fileID := photos[len(photos)-1].FileID

	fileURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", b.token, fileID)
	resp2, err := http.Get(fileURL)
	if err != nil {
		return ""
	}
	defer resp2.Body.Close()

	var fileResp GetFileResponse
	if err := json.NewDecoder(resp2.Body).Decode(&fileResp); err != nil || !fileResp.OK {
		return ""
	}

	return fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.token, fileResp.Result.FilePath)
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
