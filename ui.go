package main

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/sahilm/fuzzy"
)

// viewMode tracks what the UI is showing.
type viewMode int

const (
	modeNormal viewMode = iota
	modeFilter
	modeHelp
)

// flashMsg is used to clear the status flash after a delay.
type flashMsg struct{}

// Model is the main bubbletea model.
type Model struct {
	cfg     *Config
	entries []Entry
	// filtered holds indices into entries for the current filter.
	filtered []int
	cursor   int

	filter textinput.Model
	mode   viewMode

	tunnels *TunnelManager

	width  int
	height int

	discovering bool
	discErrors  []hostError

	flash string // ephemeral status message
}

func newModel(cfg *Config) Model {
	ti := textinput.New()
	ti.Placeholder = "type to filter..."
	ti.CharLimit = 64

	return Model{
		cfg:     cfg,
		tunnels: NewTunnelManager(),
		filter:  ti,
		mode:    modeNormal,
	}
}

// --- Fuzzy matching ---

type entrySource struct {
	entries []Entry
	indices []int
}

func (s entrySource) String(i int) string {
	e := s.entries[s.indices[i]]
	return fmt.Sprintf("%s %s %s", e.Env, e.Host, e.Tenant)
}

func (s entrySource) Len() int { return len(s.indices) }

// --- Init ---

func (m Model) Init() tea.Cmd {
	return discoverAll(m.cfg)
}

// --- Update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case discoverMsg:
		m.discovering = false
		m.entries = msg.entries
		m.discErrors = msg.errors
		m.resetFilter()
		// Trigger autoconnect.
		cmds = append(cmds, m.autoconnect()...)

	case tunnelConnectedMsg:
		if e := m.findEntry(msg.key); e != nil {
			e.Status = StatusConnected
			e.Error = ""
			cmds = append(cmds, m.tunnels.MonitorTunnel(msg.key))
		}

	case tunnelErrorMsg:
		if e := m.findEntry(msg.key); e != nil {
			e.Status = StatusError
			e.Error = msg.err.Error()
		}

	case tunnelDisconnectedMsg:
		if e := m.findEntry(msg.key); e != nil {
			e.Status = StatusReady
			e.Error = ""
		}

	case flashMsg:
		m.flash = ""

	case tea.KeyPressMsg:
		switch m.mode {
		case modeHelp:
			cmds = append(cmds, m.updateHelp(msg)...)
		case modeFilter:
			cmds = append(cmds, m.updateFilter(msg)...)
		default:
			cmds = append(cmds, m.updateNormal(msg)...)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) updateNormal(msg tea.KeyPressMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch msg.String() {
	case "q", "ctrl+c":
		m.tunnels.DisconnectAll()
		return []tea.Cmd{tea.Quit}

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}

	case "down", "j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}

	case "c", "enter":
		if e := m.selectedEntry(); e != nil {
			switch e.Status {
			case StatusReady, StatusError:
				e.Status = StatusConnecting
				cmds = append(cmds, m.tunnels.Connect(e))
			case StatusConnected:
				e.Status = StatusReady
				cmds = append(cmds, m.tunnels.Disconnect(e))
			}
		}

	case "d":
		if e := m.selectedEntry(); e != nil && e.Status == StatusConnected {
			e.Status = StatusReady
			cmds = append(cmds, m.tunnels.Disconnect(e))
		}

	case "x":
		if e := m.selectedEntry(); e != nil && e.Status == StatusConnected {
			cmds = append(cmds, m.launchPgcli(e))
		}

	case "p":
		if e := m.selectedEntry(); e != nil {
			if err := CopyPassword(e); err != nil {
				m.flash = errorMsgStyle.Render(err.Error())
			} else {
				m.flash = flashStyle.Render("Password copied!")
			}
			cmds = append(cmds, m.clearFlashAfter(2*time.Second))
		}

	case "C":
		if e := m.selectedEntry(); e != nil {
			if e.Status != StatusConnected {
				m.flash = errorMsgStyle.Render("Connect first to copy connection string")
			} else if err := CopyConnStr(e); err != nil {
				m.flash = errorMsgStyle.Render(err.Error())
			} else {
				m.flash = flashStyle.Render("Connection string copied!")
			}
			cmds = append(cmds, m.clearFlashAfter(2*time.Second))
		}

	case "r":
		m.discovering = true
		m.entries = nil
		m.filtered = nil
		m.cursor = 0
		cmds = append(cmds, discoverAll(m.cfg))

	case "h", "?":
		m.mode = modeHelp

	case "/":
		m.mode = modeFilter
		m.filter.Focus()

	default:
		// Any printable character starts filtering (fzf-style).
		s := msg.String()
		if len(s) == 1 && s >= " " && s <= "~" {
			m.mode = modeFilter
			m.filter.Focus()
			m.filter.SetValue(s)
			m.applyFilter()
		}
	}

	return cmds
}

func (m *Model) updateFilter(msg tea.KeyPressMsg) []tea.Cmd {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.filter.Blur()
		m.filter.SetValue("")
		m.resetFilter()
		return nil

	case "enter":
		m.mode = modeNormal
		m.filter.Blur()
		return nil

	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
		return nil

	case "down", "ctrl+n":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
		return nil

	default:
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.applyFilter()
		return []tea.Cmd{cmd}
	}
}

func (m *Model) updateHelp(msg tea.KeyPressMsg) []tea.Cmd {
	switch msg.String() {
	case "esc", "h", "?", "q":
		m.mode = modeNormal
	}
	return nil
}

// --- Filter logic ---

func (m *Model) applyFilter() {
	query := m.filter.Value()
	if query == "" {
		m.resetFilter()
		return
	}

	allIndices := make([]int, len(m.entries))
	for i := range allIndices {
		allIndices[i] = i
	}

	src := entrySource{entries: m.entries, indices: allIndices}
	results := fuzzy.FindFrom(query, src)

	m.filtered = make([]int, len(results))
	for i, r := range results {
		m.filtered[i] = allIndices[r.Index]
	}

	if m.cursor >= len(m.filtered) {
		if len(m.filtered) > 0 {
			m.cursor = len(m.filtered) - 1
		} else {
			m.cursor = 0
		}
	}
}

func (m *Model) resetFilter() {
	m.filtered = make([]int, len(m.entries))
	for i := range m.filtered {
		m.filtered[i] = i
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = 0
	}
}

// --- Helpers ---

func (m *Model) selectedEntry() *Entry {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return nil
	}
	return &m.entries[m.filtered[m.cursor]]
}

func (m *Model) findEntry(key string) *Entry {
	for i := range m.entries {
		if tunnelKey(&m.entries[i]) == key {
			return &m.entries[i]
		}
	}
	return nil
}

func (m *Model) autoconnect() []tea.Cmd {
	var cmds []tea.Cmd
	for _, ac := range m.cfg.Autoconnect {
		for i := range m.entries {
			e := &m.entries[i]
			if e.Host == ac.Host && e.Tenant == ac.Tenant {
				e.Status = StatusConnecting
				cmds = append(cmds, m.tunnels.Connect(e))
			}
		}
	}
	return cmds
}

func (m *Model) launchPgcli(e *Entry) tea.Cmd {
	connStr := fmt.Sprintf("postgresql://postgres:%s@localhost:%d/%s",
		e.Password, e.LocalPort, e.Tenant)
	c := exec.Command("pgcli", connStr)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return tunnelErrorMsg{key: tunnelKey(e), err: fmt.Errorf("pgcli: %w", err)}
		}
		return nil
	})
}

func (m *Model) clearFlashAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return flashMsg{}
	})
}

// --- View ---

func altView(s string) tea.View {
	v := tea.NewView(s)
	v.AltScreen = true
	return v
}

func (m Model) View() tea.View {
	var b strings.Builder

	// Header.
	b.WriteString("\n")
	title := headerStyle.Render(" \U0001f529 DRILLBIT ")
	b.WriteString(title)
	if version != "dev" {
		b.WriteString(dimStyle.Render("  v" + version))
	}
	b.WriteString("\n\n")

	// Discovery errors.
	for _, e := range m.discErrors {
		b.WriteString("  " + errorMsgStyle.Render(fmt.Sprintf("\u2716 %s: %s", e.host, e.err)) + "\n")
	}
	if len(m.discErrors) > 0 {
		b.WriteString("\n")
	}

	if m.discovering {
		b.WriteString("  " + dimStyle.Render("Discovering databases...") + "\n")
		return altView(b.String())
	}

	if len(m.entries) == 0 && len(m.discErrors) == 0 {
		b.WriteString("  " + dimStyle.Render("No databases found.") + "\n")
		b.WriteString("  " + dimStyle.Render("Check your config: "+DefaultConfigPath()) + "\n")
		return altView(b.String())
	}

	// Filter bar.
	if m.mode == modeFilter {
		b.WriteString("  " + filterLabelStyle.Render("/") + " " + m.filter.View() + "\n\n")
	}

	// Table.
	b.WriteString(m.renderTable())
	b.WriteString("\n")

	// Flash message.
	if m.flash != "" {
		b.WriteString("  " + m.flash + "\n")
	}

	// Help overlay or help bar.
	if m.mode == modeHelp {
		b.WriteString("\n")
		b.WriteString(renderHelp())
		b.WriteString("\n")
	} else {
		b.WriteString("\n")
		b.WriteString(m.renderHelpBar())
		b.WriteString("\n")
	}

	return altView(b.String())
}

// renderTable draws the table with per-row environment coloring.
func (m Model) renderTable() string {
	if len(m.filtered) == 0 {
		return "  " + dimStyle.Render("No matches.") + "\n"
	}

	// Column widths.
	const (
		colEnv    = 6
		colHost   = 14
		colTenant = 22
		colPort   = 7
		colStatus = 30
	)

	var b strings.Builder

	// Header row.
	header := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %s",
		colEnv, "ENV",
		colHost, "HOST",
		colTenant, "TENANT",
		colPort, "PORT",
		"STATUS",
	)
	b.WriteString(colHeaderStyle.Render(header) + "\n")

	// Determine visible range for scrolling.
	maxVisible := m.height - 12 // Leave room for header, help, etc.
	if maxVisible < 5 {
		maxVisible = 5
	}

	start := 0
	if m.cursor >= maxVisible {
		start = m.cursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.filtered) {
		end = len(m.filtered)
	}

	for i := start; i < end; i++ {
		idx := m.filtered[i]
		e := m.entries[idx]
		isSelected := i == m.cursor

		env := strings.ToUpper(e.Env)
		status := styledStatus(e.Status, e.Error)

		row := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*d",
			colEnv, env,
			colHost, e.Host,
			colTenant, e.Tenant,
			colPort, e.LocalPort,
		)

		if isSelected {
			// Render the row content with selection highlight, status appended.
			b.WriteString(selectedStyle.Render(row) + "  " + status + "\n")
		} else {
			style := envRowStyle(e.Env)
			b.WriteString(style.Render(row) + "  " + status + "\n")
		}
	}

	// Scroll indicator.
	if len(m.filtered) > maxVisible {
		total := len(m.filtered)
		b.WriteString(dimStyle.Render(fmt.Sprintf("  ... %d / %d shown", end-start, total)) + "\n")
	}

	return b.String()
}

// renderHelpBar renders the compact keybinding bar at the bottom.
func (m Model) renderHelpBar() string {
	keys := []struct{ key, desc string }{
		{"c", "connect"},
		{"d", "disconnect"},
		{"x", "pgcli"},
		{"p", "password"},
		{"C", "connstr"},
		{"/", "filter"},
		{"r", "refresh"},
		{"h", "help"},
		{"q", "quit"},
	}

	var parts []string
	for _, k := range keys {
		parts = append(parts, helpKeyStyle.Render(k.key)+helpBarStyle.Render(":"+k.desc))
	}
	return "  " + lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(parts, "  "))
}
