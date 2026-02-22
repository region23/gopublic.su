package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopublic/internal/client/events"
	"gopublic/internal/client/stats"
	"gopublic/internal/client/updater"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Version can be set at build time
var Version = "dev"

// TunnelInfo represents info about a single tunnel
type TunnelInfo struct {
	Name         string
	LocalPort    string
	BoundDomains []string
	Scheme       string
}

// RequestEntry represents a recent request for display
type RequestEntry struct {
	Method   string
	Path     string
	Status   int
	Duration time.Duration
	Time     time.Time
}

// LogEntry represents a log message for display
type LogEntry struct {
	Level   string
	Message string
	Time    time.Time
}

// Model is the main Bubble Tea model
type Model struct {
	// Connection state
	status string // "connecting", "online", "reconnecting", "offline"

	// Detailed connection status
	connectionStage   string // Current stage of connection
	connectionMessage string // Human-readable connection status message

	// Tunnel information
	tunnels []TunnelInfo

	// Dependencies
	stats    *stats.Stats
	eventBus *events.Bus
	eventSub <-chan events.Event

	// Display state
	width     int
	height    int
	startTime time.Time

	// Server info
	serverAddr    string
	serverLatency time.Duration

	// Recent requests for display
	requests    []RequestEntry
	maxRequests int

	// Error message (if any)
	lastError string

	// Log messages for display
	logs    []LogEntry
	maxLogs int

	// Update state
	updateInfo     *updater.UpdateInfo
	updateChecked  bool
	updateStatus   string // "", "checking", "downloading", "done", "error"
	updateMessage  string

	// Server bandwidth stats (initial values from server)
	serverBandwidthToday int64
	serverBandwidthTotal int64
	serverBandwidthLimit int64

	// Session bandwidth (accumulated during this session)
	sessionBandwidth int64
}

// NewModel creates a new TUI model
func NewModel(eventBus *events.Bus, statsTracker *stats.Stats) Model {
	var eventSub <-chan events.Event
	if eventBus != nil {
		eventSub = eventBus.Subscribe()
	}

	return Model{
		status:      "connecting",
		tunnels:     make([]TunnelInfo, 0),
		stats:       statsTracker,
		eventBus:    eventBus,
		eventSub:    eventSub,
		startTime:   time.Now(),
		requests:    make([]RequestEntry, 0),
		maxRequests: 10,
		logs:        make([]LogEntry, 0),
		maxLogs:     5,
	}
}

// Messages
type tickMsg time.Time
type eventMsg events.Event
type updateCheckMsg struct {
	info *updater.UpdateInfo
	err  error
}
type updateResultMsg struct {
	result *updater.UpdateResult
	err    error
}

// Commands
func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func waitForEvent(sub <-chan events.Event) tea.Cmd {
	return func() tea.Msg {
		if sub == nil {
			return nil
		}
		event, ok := <-sub
		if !ok {
			return nil
		}
		return eventMsg(event)
	}
}

func checkForUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info, err := updater.CheckForUpdate(ctx, Version)
		return updateCheckMsg{info: info, err: err}
	}
}

func performUpdateCmd(info *updater.UpdateInfo) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		result, err := updater.PerformUpdate(ctx, info)
		return updateResultMsg{result: result, err: err}
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickCmd(), checkForUpdateCmd()}
	if m.eventSub != nil {
		cmds = append(cmds, waitForEvent(m.eventSub))
	}
	return tea.Batch(cmds...)
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "ctrl+u", "u":
			// Trigger update if available
			if m.updateInfo != nil && m.updateInfo.Available && m.updateStatus == "" {
				m.updateStatus = "downloading"
				m.updateMessage = "Downloading update..."
				return m, performUpdateCmd(m.updateInfo)
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		// Refresh stats display
		return m, tickCmd()

	case eventMsg:
		m = m.handleEvent(events.Event(msg))
		return m, waitForEvent(m.eventSub)

	case updateCheckMsg:
		m.updateChecked = true
		if msg.err != nil {
			// Silently ignore update check errors
			return m, nil
		}
		m.updateInfo = msg.info
		return m, nil

	case updateResultMsg:
		if msg.err != nil {
			m.updateStatus = "error"
			m.updateMessage = msg.err.Error()
		} else if msg.result != nil {
			m.updateStatus = "done"
			m.updateMessage = msg.result.Message
		}
		return m, nil
	}

	return m, nil
}

func (m Model) handleEvent(event events.Event) Model {
	switch event.Type {
	case events.EventConnecting:
		m.status = "connecting"
		m.connectionStage = ""
		m.connectionMessage = ""

	case events.EventConnected:
		m.status = "online"
		m.connectionStage = ""
		m.connectionMessage = ""
		// Clear connection-related logs (e.g., "Connecting to...")
		m.logs = nil
		if data, ok := event.Data.(events.ConnectedData); ok {
			m.serverAddr = data.ServerAddr
			m.serverLatency = data.Latency
			m.serverBandwidthToday = data.BandwidthToday
			m.serverBandwidthTotal = data.BandwidthTotal
			m.serverBandwidthLimit = data.BandwidthLimit
		}

	case events.EventDisconnected:
		m.status = "offline"
		m.connectionStage = ""
		m.connectionMessage = ""

	case events.EventReconnecting:
		m.status = "reconnecting"

	case events.EventConnectionStatus:
		if data, ok := event.Data.(events.ConnectionStatusData); ok {
			m.connectionStage = data.Stage
			m.connectionMessage = data.Message
		}

	case events.EventTunnelReady:
		if data, ok := event.Data.(events.TunnelReadyData); ok {
			// Add or update tunnel info
			found := false
			for i, t := range m.tunnels {
				if t.LocalPort == data.LocalPort {
					m.tunnels[i].BoundDomains = append(m.tunnels[i].BoundDomains, data.BoundDomains...)
					found = true
					break
				}
			}
			if !found {
				m.tunnels = append(m.tunnels, TunnelInfo{
					Name:         data.Name,
					LocalPort:    data.LocalPort,
					BoundDomains: data.BoundDomains,
					Scheme:       data.Scheme,
				})
			}
		}

	case events.EventRequestComplete:
		if data, ok := event.Data.(events.RequestData); ok {
			entry := RequestEntry{
				Method:   data.Method,
				Path:     data.Path,
				Status:   data.Status,
				Duration: data.Duration,
				Time:     time.Now(),
			}
			// Prepend (newest first)
			m.requests = append([]RequestEntry{entry}, m.requests...)
			if len(m.requests) > m.maxRequests {
				m.requests = m.requests[:m.maxRequests]
			}
			// Update session bandwidth
			m.sessionBandwidth += data.Bytes
		}

	case events.EventError:
		if data, ok := event.Data.(events.ErrorData); ok {
			m.lastError = fmt.Sprintf("%s: %v", data.Context, data.Error)
			// Also add to logs
			entry := LogEntry{
				Level:   "error",
				Message: m.lastError,
				Time:    time.Now(),
			}
			m.logs = append([]LogEntry{entry}, m.logs...)
			if len(m.logs) > m.maxLogs {
				m.logs = m.logs[:m.maxLogs]
			}
		}

	case events.EventLog:
		if data, ok := event.Data.(events.LogData); ok {
			entry := LogEntry{
				Level:   data.Level,
				Message: data.Message,
				Time:    time.Now(),
			}
			m.logs = append([]LogEntry{entry}, m.logs...)
			if len(m.logs) > m.maxLogs {
				m.logs = m.logs[:m.maxLogs]
			}
		}
	}

	return m
}

// View renders the model
func (m Model) View() string {
	var b strings.Builder

	// Header
	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	// Status section
	b.WriteString(m.renderStatus())
	b.WriteString("\n")

	// Forwarding section
	if len(m.tunnels) > 0 {
		b.WriteString(m.renderForwarding())
		b.WriteString("\n")
	}

	// Stats section
	b.WriteString(m.renderStats())
	b.WriteString("\n")

	// Recent requests
	if len(m.requests) > 0 {
		b.WriteString(m.renderRequests())
	}

	// Logs section (show if there are any logs)
	if len(m.logs) > 0 {
		b.WriteString(m.renderLogs())
	}

	return b.String()
}

func (m Model) renderHeader() string {
	title := titleStyle.Render("gopublic")

	// Build hint based on update status
	var hint string
	if m.updateInfo != nil && m.updateInfo.Available && m.updateStatus == "" {
		hint = hintStyle.Render("(Ctrl+C quit, ") + updateAvailableStyle.Render("U update") + hintStyle.Render(")")
	} else {
		hint = hintStyle.Render("(Ctrl+C to quit)")
	}

	// Calculate spacing
	spacing := ""
	if m.width > 0 {
		titleLen := lipgloss.Width(title)
		hintLen := lipgloss.Width(hint)
		spaces := m.width - titleLen - hintLen
		if spaces > 0 {
			spacing = strings.Repeat(" ", spaces)
		}
	} else {
		spacing = strings.Repeat(" ", 40)
	}

	return title + spacing + hint
}

func (m Model) renderStatus() string {
	var lines []string

	// Session Status with detailed message
	statusText := StatusText(m.status)
	if m.connectionMessage != "" && m.status != "online" {
		statusText = statusText + " " + connectionDetailStyle.Render("("+m.connectionMessage+")")
	}
	lines = append(lines, m.renderField("Session Status", statusText))

	// Version with update info
	versionStr := Version
	if m.updateInfo != nil && m.updateInfo.Available {
		versionStr = Version + " " + updateAvailableStyle.Render("→ "+m.updateInfo.LatestVersion+" available")
	}
	lines = append(lines, m.renderField("Version", versionStr))

	// Update status (if downloading or completed)
	if m.updateStatus != "" {
		var updateStatusText string
		switch m.updateStatus {
		case "downloading":
			updateStatusText = updateDownloadingStyle.Render(m.updateMessage)
		case "done":
			updateStatusText = updateDoneStyle.Render(m.updateMessage)
		case "error":
			updateStatusText = updateErrorStyle.Render(m.updateMessage)
		}
		lines = append(lines, m.renderField("Update", updateStatusText))
	}

	// Latency
	latencyStr := "-"
	if m.serverLatency > 0 {
		latencyStr = fmt.Sprintf("%dms", m.serverLatency.Milliseconds())
	}
	lines = append(lines, m.renderField("Latency", latencyStr))

	// Web Interface
	lines = append(lines, m.renderField("Web Interface", urlStyle.Render("http://127.0.0.1:4040")))

	return strings.Join(lines, "\n")
}

func (m Model) renderField(label, value string) string {
	return labelStyle.Render(label) + valueStyle.Render(value)
}

func (m Model) renderForwarding() string {
	var lines []string
	lines = append(lines, "") // Empty line before

	for i, t := range m.tunnels {
		for j, domain := range t.BoundDomains {
			label := ""
			if i == 0 && j == 0 {
				label = "Forwarding"
			}

			url := fmt.Sprintf("%s://%s", t.Scheme, domain)
			localAddr := t.LocalPort
			if !strings.Contains(localAddr, ":") {
				localAddr = "localhost:" + localAddr
			}
			local := fmt.Sprintf("http://%s", localAddr)

			value := urlStyle.Render(url) + arrowStyle.Render(" -> ") + valueStyle.Render(local)
			lines = append(lines, labelStyle.Render(label)+value)
		}
	}

	return strings.Join(lines, "\n")
}

func (m Model) renderStats() string {
	var lines []string
	lines = append(lines, "") // Empty line before

	// Get stats snapshot
	var snap stats.Snapshot
	if m.stats != nil {
		snap = m.stats.Snapshot()
	}

	// Header row
	headers := []string{"ttl", "opn", "rt1", "rt5", "p50", "p90"}
	headerRow := labelStyle.Render("Connections")
	for _, h := range headers {
		headerRow += statsHeaderStyle.Render(h)
	}
	lines = append(lines, headerRow)

	// Values row
	valueRow := labelStyle.Render("")
	valueRow += statsValueStyle.Render(fmt.Sprintf("%d", snap.TotalConnections))
	valueRow += statsValueStyle.Render(fmt.Sprintf("%d", snap.OpenConnections))
	valueRow += statsValueStyle.Render(formatDuration(snap.RT1))
	valueRow += statsValueStyle.Render(formatDuration(snap.RT5))
	valueRow += statsValueStyle.Render(formatDuration(snap.P50))
	valueRow += statsValueStyle.Render(formatDuration(snap.P90))
	lines = append(lines, valueRow)

	// Bandwidth stats from server (if available)
	if m.serverBandwidthLimit > 0 {
		lines = append(lines, "")
		bandwidthLine := labelStyle.Render("Bandwidth") +
			statsHeaderStyle.Render("today") +
			statsHeaderStyle.Render("total") +
			statsHeaderStyle.Render("limit")
		lines = append(lines, bandwidthLine)

		// Calculate current bandwidth: server initial + session accumulated
		currentToday := m.serverBandwidthToday + m.sessionBandwidth
		currentTotal := m.serverBandwidthTotal + m.sessionBandwidth

		bandwidthValueRow := labelStyle.Render("") +
			statsValueStyle.Render(formatBytesShort(currentToday)) +
			statsValueStyle.Render(formatBytesShort(currentTotal)) +
			statsValueStyle.Render(formatBytesShort(m.serverBandwidthLimit))
		lines = append(lines, bandwidthValueRow)
	}

	return strings.Join(lines, "\n")
}

func (m Model) renderRequests() string {
	var lines []string
	lines = append(lines, "") // Empty line before
	lines = append(lines, labelStyle.Render("HTTP Requests"))

	for _, req := range m.requests {
		method := MethodText(req.Method)
		path := pathStyle.Render(truncatePath(req.Path, 40))
		status := StatusCodeText(req.Status)
		duration := durationStyle.Render(formatDuration(req.Duration))

		line := fmt.Sprintf("%s %s %s %s", method, path, status, duration)
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func (m Model) renderLogs() string {
	var lines []string
	lines = append(lines, "") // Empty line before
	lines = append(lines, labelStyle.Render("Logs"))

	for _, log := range m.logs {
		var levelStyle lipgloss.Style
		switch log.Level {
		case "error":
			levelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // Red
		case "warn":
			levelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // Yellow
		default:
			levelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // Gray
		}

		// Wrap long messages based on terminal width
		msg := log.Message
		maxLen := 70
		if m.width > 20 {
			maxLen = m.width - 4 // Leave some margin
		}

		// Word wrap long messages
		wrappedLines := wrapText(msg, maxLen)
		for _, wl := range wrappedLines {
			lines = append(lines, levelStyle.Render(wl))
		}
	}

	return strings.Join(lines, "\n")
}

// Helper functions

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0.00"
	}
	secs := d.Seconds()
	if secs < 1 {
		return fmt.Sprintf("%.2f", secs)
	}
	return fmt.Sprintf("%.1f", secs)
}

func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	return path[:maxLen-3] + "..."
}

func formatBytesShort(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.0fK", float64(bytes)/1024)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1fM", float64(bytes)/(1024*1024))
	}
	return fmt.Sprintf("%.1fG", float64(bytes)/(1024*1024*1024))
}

// wrapText wraps text to fit within maxLen characters per line.
func wrapText(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 70
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	var lines []string
	for len(text) > maxLen {
		// Find last space within maxLen
		breakPoint := maxLen
		for i := maxLen - 1; i > 0; i-- {
			if text[i] == ' ' {
				breakPoint = i
				break
			}
		}
		lines = append(lines, text[:breakPoint])
		// Skip the space if we broke at one
		if breakPoint < len(text) && text[breakPoint] == ' ' {
			breakPoint++
		}
		text = text[breakPoint:]
	}
	if len(text) > 0 {
		lines = append(lines, text)
	}
	return lines
}

// Run starts the TUI application
func Run(eventBus *events.Bus, statsTracker *stats.Stats) error {
	model := NewModel(eventBus, statsTracker)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
