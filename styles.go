package main

import (
	"hash/fnv"
	"strings"

	"charm.land/lipgloss/v2"
)

// envPalette is a set of visually distinct, readable-on-dark-background hex colors
// used for deterministic environment label coloring.
var envPalette = []string{
	"#FF6B6B", // red
	"#51CF66", // green
	"#74C0FC", // blue
	"#FFA94D", // orange
	"#DA77F2", // purple
	"#20C997", // teal
	"#FFD43B", // yellow
	"#F06595", // pink
	"#38D9A9", // mint
	"#748FFC", // indigo
}

// envColor returns a deterministic color for an environment label.
func envColor(env string) lipgloss.Style {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(env)))
	hex := envPalette[h.Sum32()%uint32(len(envPalette))]
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
}

var (
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

	// Wider overlay for restore confirmation.
	wideOverlayStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#e94560")).
				Padding(1, 3).
				Width(76)

	helpTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#e94560")).
			MarginBottom(1)

	// Update available indicator.
	updateAvailableStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#51CF66")).
				Bold(true)

	// Discovery log tag styles.
	logTagConn = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD93D")).Bold(true)
	logTagOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("#51CF66")).Bold(true)
	logTagScan = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD93D")).Bold(true)
	logTagFind = lipgloss.NewStyle().Foreground(lipgloss.Color("#74C0FC")).Bold(true)
	logTagErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B")).Bold(true)
	logTagDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555"))
	logText    = lipgloss.NewStyle().Foreground(lipgloss.Color("#AAAAAA"))
)

// envRowStyle returns the style for a row based on its environment label.
// Uses a deterministic hash for stable color assignment.
func envRowStyle(env string) lipgloss.Style {
	if env == "" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	}
	return envColor(env)
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

// renderLogEntry renders a discovery log line with a color-coded tag.
func renderLogEntry(e logEntry) string {
	var tag string
	switch e.tag {
	case "CONN":
		tag = logTagConn.Render("[CONN]")
	case "OK":
		tag = logTagOK.Render("[ OK ]")
	case "SCAN":
		tag = logTagScan.Render("[SCAN]")
	case "FIND":
		tag = logTagFind.Render("[FIND]")
	case "ERR":
		tag = logTagErr.Render("[ ERR]")
	default:
		tag = logTagDim.Render("[    ]")
	}
	return "  " + tag + " " + logText.Render(e.text)
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
