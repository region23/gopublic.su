package tui

import "github.com/charmbracelet/lipgloss"

// Color palette - inspired by ngrok's terminal UI
var (
	// Primary colors
	colorGreen  = lipgloss.Color("40")  // Bright green for "online"
	colorYellow = lipgloss.Color("220") // Yellow for "connecting"
	colorRed    = lipgloss.Color("196") // Red for errors
	colorCyan   = lipgloss.Color("39")  // Cyan for URLs
	colorGray   = lipgloss.Color("244") // Gray for labels
	colorWhite  = lipgloss.Color("255") // White for values
	colorDim    = lipgloss.Color("240") // Dim gray for secondary text
)

// Text styles
var (
	// Title style for "gopublic" header
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite)

	// Hint style for "(Ctrl+C to quit)"
	hintStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	// Label style for field names
	labelStyle = lipgloss.NewStyle().
			Width(20).
			Foreground(colorGray)

	// Value style for field values
	valueStyle = lipgloss.NewStyle().
			Foreground(colorWhite)

	// Status styles
	statusOnlineStyle = lipgloss.NewStyle().
				Foreground(colorGreen).
				Bold(true)

	statusConnectingStyle = lipgloss.NewStyle().
				Foreground(colorYellow)

	statusOfflineStyle = lipgloss.NewStyle().
				Foreground(colorRed)

	// URL style for forwarding URLs
	urlStyle = lipgloss.NewStyle().
			Foreground(colorCyan)

	// Arrow style for forwarding display
	arrowStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	// Stats header style
	statsHeaderStyle = lipgloss.NewStyle().
				Foreground(colorGray).
				Width(8)

	// Stats value style
	statsValueStyle = lipgloss.NewStyle().
			Foreground(colorWhite).
			Width(8)

	// Section style with padding
	sectionStyle = lipgloss.NewStyle().
			PaddingLeft(0)

	// Request log styles
	methodGetStyle = lipgloss.NewStyle().
			Foreground(colorGreen).
			Width(7)

	methodPostStyle = lipgloss.NewStyle().
			Foreground(colorYellow).
			Width(7)

	methodOtherStyle = lipgloss.NewStyle().
				Foreground(colorCyan).
				Width(7)

	statusOKStyle = lipgloss.NewStyle().
			Foreground(colorGreen)

	statusErrorStyle = lipgloss.NewStyle().
				Foreground(colorRed)

	pathStyle = lipgloss.NewStyle().
			Foreground(colorWhite)

	durationStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	// Update available style
	updateAvailableStyle = lipgloss.NewStyle().
				Foreground(colorGreen).
				Bold(true)

	// Update status styles
	updateDownloadingStyle = lipgloss.NewStyle().
				Foreground(colorYellow)

	updateErrorStyle = lipgloss.NewStyle().
				Foreground(colorRed)

	updateDoneStyle = lipgloss.NewStyle().
			Foreground(colorGreen)
)

// StatusText returns styled status text
func StatusText(status string) string {
	switch status {
	case "online":
		return statusOnlineStyle.Render("online")
	case "connecting":
		return statusConnectingStyle.Render("connecting")
	case "reconnecting":
		return statusConnectingStyle.Render("reconnecting")
	case "offline":
		return statusOfflineStyle.Render("offline")
	default:
		return valueStyle.Render(status)
	}
}

// MethodText returns styled HTTP method
func MethodText(method string) string {
	switch method {
	case "GET":
		return methodGetStyle.Render(method)
	case "POST":
		return methodPostStyle.Render(method)
	default:
		return methodOtherStyle.Render(method)
	}
}

// StatusCodeText returns styled HTTP status code
func StatusCodeText(code int) string {
	if code >= 200 && code < 400 {
		return statusOKStyle.Render(formatInt(code))
	}
	return statusErrorStyle.Render(formatInt(code))
}

func formatInt(n int) string {
	return lipgloss.NewStyle().Render(intToString(n))
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
