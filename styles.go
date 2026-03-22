package main

import (
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	// Environment colors.
	prodColor  = lipgloss.Color("#FF6B6B")
	testColor  = lipgloss.Color("#51CF66")
	otherColor = lipgloss.Color("#74C0FC")

	// Image type colors.
	postgresColor   = lipgloss.Color("#336791") // Official Postgres blue
	postgisColor    = lipgloss.Color("#51A7DB") // Lighter blue for PostGIS
	timescaleColor  = lipgloss.Color("#FDB515") // Timescale orange
	unknownImgColor = lipgloss.Color("#666666") // Gray for unknown

	// Header banner.
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1a1a2e")).
			Background(lipgloss.Color("#e94560")).
			Padding(0, 1)

	headerAccent = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#e94560"))

	// Column headers.
	colHeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#888888")).
			Underline(true)

	// Row styles.
	prodRowStyle = lipgloss.NewStyle().
			Foreground(prodColor)

	testRowStyle = lipgloss.NewStyle().
			Foreground(testColor)

	otherRowStyle = lipgloss.NewStyle().
			Foreground(otherColor)

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Background(lipgloss.Color("#333366")).
			Foreground(lipgloss.Color("#FFFFFF"))

	// Status styles.
	statusReady = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#666666"))

	statusConnecting = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFD93D"))

	statusConnected = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#51CF66")).
			Bold(true)

	statusError = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF6B6B")).
			Bold(true)

	// Help bar at bottom.
	helpBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#666666"))

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#e94560")).
			Bold(true)

	// Status message flash.
	flashStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFD93D")).
			Bold(true)

	// Error display.
	errorMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF6B6B"))

	// Filter bar.
	filterLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#e94560")).
				Bold(true)

	// Dim text.
	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555"))

	// Help overlay.
	helpOverlayStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#e94560")).
				Padding(1, 2).
				Width(64)

	helpTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#e94560")).
			MarginBottom(1)

	// Update available indicator.
	updateAvailableStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#51CF66")).
				Bold(true)
)

// envRowStyle returns the style for a row based on its environment.
func envRowStyle(env string) lipgloss.Style {
	switch ClassifyEnv(env) {
	case EnvProd:
		return prodRowStyle
	case EnvTest:
		return testRowStyle
	default:
		return otherRowStyle
	}
}

// styledStatus returns the styled status string.
func styledStatus(s Status) string {
	switch s {
	case StatusReady:
		return statusReady.Render("\u25cb Ready")
	case StatusConnecting:
		return statusConnecting.Render("\u25cc Connecting...")
	case StatusConnected:
		return statusConnected.Render("\u25cf Connected")
	case StatusError:
		return statusError.Render("\u2716 Error")
	default:
		return "?"
	}
}

// imageTypeBadge returns a color-coded badge for the database image type.
func imageTypeBadge(image string, width int) string {
	var style lipgloss.Style
	var label string

	switch image {
	case "postgres":
		style = lipgloss.NewStyle().Foreground(postgresColor).Bold(true)
		label = "pg"
	case "postgis":
		style = lipgloss.NewStyle().Foreground(postgisColor).Bold(true)
		label = "gis"
	case "timescale":
		style = lipgloss.NewStyle().Foreground(timescaleColor).Bold(true)
		label = "time"
	default:
		style = lipgloss.NewStyle().Foreground(unknownImgColor).Bold(true)
		label = "?"
	}

	badge := style.Render(label)

	// Pad to width if needed
	if len(label) < width {
		badge += lipgloss.NewStyle().Foreground(lipgloss.Color("#333333")).Render(strings.Repeat(" ", width-len(label)))
	}

	return badge
}
