package main

import (
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/sahilm/fuzzy"
)

// viewMode tracks what the UI is showing.
type viewMode int

const (
	modeNormal viewMode = iota
	modeFilter
	modeHelp
	modeCopy
	modeConfirmQuit
	modeConfig
	modeEdit     // edit selected entry (user, pw, db, autoconnect)
	modeQuitting // dissolve animation during shutdown
)

// flashMsg is used to clear the status flash after a delay.
type flashMsg struct{}

// clipboardClearMsg is sent to wipe the clipboard after a timeout.
type clipboardClearMsg struct{}

type dissolveTickMsg struct{}
type shutdownDoneMsg struct{}

// dissolveState tracks the shutdown dissolve animation.
type dissolveState struct {
	grid     [][]rune
	cells    [][2]int // non-space cells in random dissolve order
	pos      int
	batchSz  int
	shutdown bool
}

// sqlClientErrorMsg is sent when pgcli/psql exits with an error.
// Separate from tunnelErrorMsg so the tunnel stays connected.
type sqlClientErrorMsg struct{ err error }

// tunnelHealthMsg is sent periodically to check tunnel health.
type tunnelHealthMsg struct{}

const healthCheckInterval = 30 * time.Second

// Model is the main bubbletea model.
type Model struct {
	cfg        *Config
	configPath string
	entries    []Entry
	// filtered holds indices into entries for the current filter.
	filtered []int
	cursor   int

	filterText string
	mode       viewMode

	tunnels *TunnelManager
	editor  configEditor

	// Edit mode fields (for editing selected entry's user/pw/db).
	editFields      [3]textinput.Model
	editFieldCursor int
	editAutoconnect bool // toggle in edit form

	width  int
	height int

	discovering bool
	discErrors  []hostError

	flash         string // ephemeral status message
	tagline       string // random tagline picked at startup
	sqlClient     string // "pgcli", "psql", or "" if neither found
	pendingLaunch string // tunnel key to auto-launch SQL client once connected

	// Update tracking.
	updateAvailable *updateInfo
	updating        bool

	// Shutdown dissolve animation.
	dissolve *dissolveState
}

func newModel(cfg *Config, configPath string) Model {
	// Detect SQL client: prefer pgcli, fall back to psql.
	var sqlClient string
	if _, err := exec.LookPath("pgcli"); err == nil {
		sqlClient = "pgcli"
	} else if _, err := exec.LookPath("psql"); err == nil {
		sqlClient = "psql"
	}

	return Model{
		cfg:        cfg,
		configPath: configPath,
		tunnels:    NewTunnelManager(),
		mode:       modeNormal,
		tagline:    randomTagline(),
		sqlClient:  sqlClient,
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
	return tea.Batch(
		discoverAll(m.cfg),
		checkForUpdate(),
		m.scheduleHealthCheck(),
	)
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
		m.discErrors = msg.errors

		// Build a map of existing entry state keyed by host:tenant.
		existing := make(map[string]*Entry, len(m.entries))
		for i := range m.entries {
			existing[tunnelKey(&m.entries[i])] = &m.entries[i]
		}

		// Merge: carry over status, error, and container IP from active tunnels.
		for i := range msg.entries {
			key := tunnelKey(&msg.entries[i])
			if old, ok := existing[key]; ok {
				msg.entries[i].Status = old.Status
				msg.entries[i].Error = old.Error
				msg.entries[i].ContainerIP = old.ContainerIP
				msg.entries[i].LocalPort = old.LocalPort // keep the same port
			}
		}

		firstRun := len(m.entries) == 0
		m.entries = msg.entries
		m.applyOverrides()
		m.applyFilter()

		// Only autoconnect on the first discovery, not refreshes.
		if firstRun {
			cmds = append(cmds, m.autoconnect()...)
		}

	case tunnelConnectedMsg:
		if e := m.findEntry(msg.key); e != nil {
			e.Status = StatusConnected
			e.Error = ""
			cmds = append(cmds, m.tunnels.MonitorTunnel(msg.key))
			// If Enter was pressed on a disconnected entry, auto-launch SQL client now.
			if m.pendingLaunch == msg.key && m.sqlClient != "" {
				m.pendingLaunch = ""
				cmds = append(cmds, m.launchSQLClient(e))
			}
		}

	case tunnelErrorMsg:
		if e := m.findEntry(msg.key); e != nil {
			e.Status = StatusError
			e.Error = msg.err.Error()
		}
		// Clear pending launch if the tunnel we were waiting for failed.
		if m.pendingLaunch == msg.key {
			m.pendingLaunch = ""
		}

	case sqlClientErrorMsg:
		// SQL client exited with error — flash it but leave connection intact.
		m.flash = errorMsgStyle.Render(msg.err.Error())
		cmds = append(cmds, m.clearFlashAfter(3*time.Second))

	case tunnelDisconnectedMsg:
		if e := m.findEntry(msg.key); e != nil {
			e.Status = StatusReady
			e.Error = ""
		}

	case updateAvailableMsg:
		m.updateAvailable = &msg.info

	case updateDoneMsg:
		m.updating = false
		if msg.err != nil {
			m.flash = errorMsgStyle.Render(fmt.Sprintf("Update failed: %v", msg.err))
			cmds = append(cmds, m.clearFlashAfter(5*time.Second))
		} else {
			m.flash = flashStyle.Render(fmt.Sprintf("Updated to v%s — restart to use new version", msg.newVersion))
			m.updateAvailable = nil
			cmds = append(cmds, m.clearFlashAfter(10*time.Second))
		}

	case tunnelHealthMsg:
		cmds = append(cmds, m.checkTunnelHealth()...)
		cmds = append(cmds, m.scheduleHealthCheck())

	case clipboardClearMsg:
		clipboard.WriteAll("")

	case dissolveTickMsg:
		if d := m.dissolve; d != nil {
			end := min(d.pos+d.batchSz, len(d.cells))
			for i := d.pos; i < end; i++ {
				r, c := d.cells[i][0], d.cells[i][1]
				if r < len(d.grid) && c < len(d.grid[r]) {
					d.grid[r][c] = ' '
				}
			}
			d.pos = end
			if d.pos >= len(d.cells) && d.shutdown {
				return m, tea.Quit
			}
			cmds = append(cmds, tea.Tick(35*time.Millisecond, func(time.Time) tea.Msg {
				return dissolveTickMsg{}
			}))
		}

	case shutdownDoneMsg:
		if m.dissolve != nil {
			m.dissolve.shutdown = true
			if m.dissolve.pos >= len(m.dissolve.cells) {
				return m, tea.Quit
			}
		}

	case flashMsg:
		m.flash = ""

	case tea.PasteMsg:
		switch m.mode {
		case modeEdit:
			if m.editFieldCursor < 3 {
				var cmd tea.Cmd
				m.editFields[m.editFieldCursor], cmd = m.editFields[m.editFieldCursor].Update(msg)
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		case modeConfig:
			cmd := m.editor.updatePaste(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case tea.KeyPressMsg:
		switch m.mode {
		case modeQuitting:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		case modeConfig:
			done, save, cmd := m.editor.update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if done {
				if save {
					overrides := m.cfg.Overrides
					m.cfg = m.editor.toConfig(m.cfg.Autoconnect)
					m.cfg.Overrides = overrides
					if err := SaveConfig(m.cfg, m.configPath); err != nil {
						m.flash = errorMsgStyle.Render(fmt.Sprintf("Save: %v", err))
					} else {
						m.flash = flashStyle.Render("Config saved, refreshing...")
						m.discovering = true
						cmds = append(cmds, discoverAll(m.cfg))
					}
					cmds = append(cmds, m.clearFlashAfter(2*time.Second))
				}
				m.mode = modeNormal
			}
		case modeEdit:
			cmds = append(cmds, m.updateEdit(msg)...)
		case modeConfirmQuit:
			cmds = append(cmds, m.updateConfirmQuit(msg)...)
		case modeHelp:
			cmds = append(cmds, m.updateHelp(msg)...)
		case modeCopy:
			cmds = append(cmds, m.updateCopy(msg)...)
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
	case "ctrl+c":
		if m.activeConnectionCount() > 0 {
			return m.startDissolve()
		}
		return []tea.Cmd{tea.Quit}

	case "esc":
		if m.filterText != "" {
			// First Esc clears any active filter.
			m.filterText = ""
			m.applyFilter()
		} else if m.activeConnectionCount() > 0 {
			m.mode = modeConfirmQuit
		} else {
			m.tunnels.DisconnectAll()
			return []tea.Cmd{tea.Quit}
		}

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}

	case "down", "j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}

	case "space":
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

	case "enter":
		if e := m.selectedEntry(); e != nil {
			// Guard: require user, password, and database to be set.
			if e.DBUser == "" || e.Password == "" || e.Database == "" {
				var missing []string
				if e.DBUser == "" {
					missing = append(missing, "user")
				}
				if e.Password == "" {
					missing = append(missing, "password")
				}
				if e.Database == "" {
					missing = append(missing, "database")
				}
				m.flash = errorMsgStyle.Render(fmt.Sprintf("Missing %s — press c to configure", strings.Join(missing, ", ")))
				cmds = append(cmds, m.clearFlashAfter(3*time.Second))
			} else if e.Status == StatusConnected && m.sqlClient != "" {
				cmds = append(cmds, m.launchSQLClient(e))
			} else if e.Status == StatusReady || e.Status == StatusError {
				e.Status = StatusConnecting
				// If SQL client available, auto-launch once connected.
				if m.sqlClient != "" {
					m.pendingLaunch = tunnelKey(e)
				}
				cmds = append(cmds, m.tunnels.Connect(e))
			}
		}

	case "y":
		if m.selectedEntry() != nil {
			m.mode = modeCopy
		}

	case "c":
		if e := m.selectedEntry(); e != nil {
			m.startEdit(e)
		}

	case "h":
		m.editor = newConfigEditor(m.cfg)
		m.mode = modeConfig

	case "u":
		if m.updateAvailable != nil && !m.updating {
			m.updating = true
			m.flash = flashStyle.Render("Downloading update...")
			cmds = append(cmds, performUpdate(*m.updateAvailable))
		}

	case "r":
		m.discovering = true
		cmds = append(cmds, discoverAll(m.cfg))

	case "?":
		m.mode = modeHelp

	case "/":
		m.mode = modeFilter
	}

	return cmds
}

// startEdit initializes the edit form for the selected entry.
func (m *Model) startEdit(e *Entry) {
	m.editFieldCursor = 0
	m.editAutoconnect = m.isAutoconnect(e)

	m.editFields[0] = textinput.New()
	m.editFields[0].Placeholder = "postgres"
	m.editFields[0].CharLimit = 64
	m.editFields[0].SetWidth(30)
	m.editFields[0].SetValue(e.DBUser)

	m.editFields[1] = textinput.New()
	m.editFields[1].Placeholder = "(password)"
	m.editFields[1].CharLimit = 256
	m.editFields[1].SetWidth(30)
	m.editFields[1].SetValue(e.Password)

	m.editFields[2] = textinput.New()
	m.editFields[2].Placeholder = e.Tenant
	m.editFields[2].CharLimit = 128
	m.editFields[2].SetWidth(30)
	m.editFields[2].SetValue(e.Database)

	m.editFields[0].Focus()
	m.mode = modeEdit
}

func (m *Model) updateEdit(msg tea.KeyPressMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch msg.String() {
	case "ctrl+c", "esc":
		if m.editFieldCursor < 3 {
			m.editFields[m.editFieldCursor].Blur()
		}
		m.mode = modeNormal

	case "enter":
		e := m.selectedEntry()
		if e == nil {
			m.mode = modeNormal
			break
		}

		user := strings.TrimSpace(m.editFields[0].Value())
		pw := strings.TrimSpace(m.editFields[1].Value())
		db := strings.TrimSpace(m.editFields[2].Value())

		if user == "" {
			user = "postgres"
		}
		if db == "" {
			db = e.Tenant
		}

		e.DBUser = user
		e.Password = pw
		e.Database = db

		// Save override to config.
		key := tunnelKey(e)
		m.cfg.Overrides[key] = EntryOverride{
			DBUser:   user,
			Password: pw,
			Database: db,
		}

		// Update autoconnect.
		wasAuto := m.isAutoconnect(e)
		if m.editAutoconnect && !wasAuto {
			m.toggleAutoconnect(e)
		} else if !m.editAutoconnect && wasAuto {
			m.toggleAutoconnect(e)
		}

		if err := SaveConfig(m.cfg, m.configPath); err != nil {
			m.flash = errorMsgStyle.Render(fmt.Sprintf("Save: %v", err))
		} else {
			m.flash = flashStyle.Render("Saved")
		}
		cmds = append(cmds, m.clearFlashAfter(2*time.Second))

		if m.editFieldCursor < 3 {
			m.editFields[m.editFieldCursor].Blur()
		}
		m.mode = modeNormal

	case "space":
		// Toggle autoconnect when on the autoconnect field.
		if m.editFieldCursor == 3 {
			m.editAutoconnect = !m.editAutoconnect
			return nil
		}
		// Otherwise pass through to textinput.
		var cmd tea.Cmd
		m.editFields[m.editFieldCursor], cmd = m.editFields[m.editFieldCursor].Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case "tab":
		if m.editFieldCursor < 3 {
			m.editFields[m.editFieldCursor].Blur()
		}
		m.editFieldCursor = (m.editFieldCursor + 1) % 4
		if m.editFieldCursor < 3 {
			m.editFields[m.editFieldCursor].Focus()
		}

	case "shift+tab":
		if m.editFieldCursor < 3 {
			m.editFields[m.editFieldCursor].Blur()
		}
		m.editFieldCursor = (m.editFieldCursor + 3) % 4
		if m.editFieldCursor < 3 {
			m.editFields[m.editFieldCursor].Focus()
		}

	default:
		if m.editFieldCursor < 3 {
			var cmd tea.Cmd
			m.editFields[m.editFieldCursor], cmd = m.editFields[m.editFieldCursor].Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	return cmds
}

func (m *Model) updateFilter(msg tea.KeyPressMsg) []tea.Cmd {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.filterText = ""
		m.resetFilter()

	case "enter":
		// Accept filter and return to normal mode (filter stays active).
		m.mode = modeNormal

	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}

	case "down", "ctrl+n":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}

	case "backspace":
		if len(m.filterText) > 0 {
			m.filterText = m.filterText[:len(m.filterText)-1]
			m.applyFilter()
		}

	default:
		s := msg.String()
		if len(s) == 1 && s >= " " && s <= "~" {
			m.filterText += s
			m.applyFilter()
		}
	}

	return nil
}

func (m *Model) updateCopy(msg tea.KeyPressMsg) []tea.Cmd {
	var cmds []tea.Cmd
	m.mode = modeNormal

	switch msg.String() {
	case "p":
		if e := m.selectedEntry(); e != nil {
			if err := CopyPassword(e); err != nil {
				m.flash = errorMsgStyle.Render(err.Error())
			} else {
				m.flash = flashStyle.Render("Password copied! (clipboard clears in 30s)")
				cmds = append(cmds, m.clearClipboardAfter(30*time.Second))
			}
			cmds = append(cmds, m.clearFlashAfter(2*time.Second))
		}
	case "c":
		if e := m.selectedEntry(); e != nil {
			if e.Status != StatusConnected {
				m.flash = errorMsgStyle.Render("Connect first to copy connection string")
			} else if err := CopyConnStr(e); err != nil {
				m.flash = errorMsgStyle.Render(err.Error())
			} else {
				m.flash = flashStyle.Render("Connection string copied! (clipboard clears in 30s)")
				cmds = append(cmds, m.clearClipboardAfter(30*time.Second))
			}
			cmds = append(cmds, m.clearFlashAfter(2*time.Second))
		}
	}

	return cmds
}

func (m *Model) updateConfirmQuit(msg tea.KeyPressMsg) []tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.startDissolve()
	default:
		m.mode = modeNormal
		return nil
	}
}

func (m *Model) updateHelp(msg tea.KeyPressMsg) []tea.Cmd {
	switch msg.String() {
	case "esc", "?":
		m.mode = modeNormal
	}
	return nil
}

// --- Filter logic ---

func (m *Model) applyFilter() {
	if m.filterText == "" {
		m.resetFilter()
		return
	}

	allIndices := make([]int, len(m.entries))
	for i := range allIndices {
		allIndices[i] = i
	}

	src := entrySource{entries: m.entries, indices: allIndices}
	results := fuzzy.FindFrom(m.filterText, src)

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

func (m *Model) activeConnectionCount() int {
	count := 0
	for _, e := range m.entries {
		if e.Status == StatusConnected || e.Status == StatusConnecting {
			count++
		}
	}
	return count
}

func (m *Model) isAutoconnect(e *Entry) bool {
	for _, ac := range m.cfg.Autoconnect {
		if ac.Host == e.Host && ac.Tenant == e.Tenant {
			return true
		}
	}
	return false
}

func (m *Model) toggleAutoconnect(e *Entry) {
	for i, ac := range m.cfg.Autoconnect {
		if ac.Host == e.Host && ac.Tenant == e.Tenant {
			m.cfg.Autoconnect = append(m.cfg.Autoconnect[:i], m.cfg.Autoconnect[i+1:]...)
			return
		}
	}
	m.cfg.Autoconnect = append(m.cfg.Autoconnect, AutoconnectEntry{Host: e.Host, Tenant: e.Tenant})
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

// applyOverrides merges config overrides into discovered entries.
func (m *Model) applyOverrides() {
	if len(m.cfg.Overrides) == 0 {
		return
	}
	for i := range m.entries {
		key := tunnelKey(&m.entries[i])
		if ov, ok := m.cfg.Overrides[key]; ok {
			if ov.DBUser != "" {
				m.entries[i].DBUser = ov.DBUser
			}
			if ov.Password != "" {
				m.entries[i].Password = ov.Password
			}
			if ov.Database != "" {
				m.entries[i].Database = ov.Database
			}
		}
	}
}

func (m *Model) launchSQLClient(e *Entry) tea.Cmd {
	connStr := fmt.Sprintf("postgresql://%s:%s@localhost:%d/%s",
		url.QueryEscape(e.DBUser), url.QueryEscape(e.Password), e.LocalPort, e.Database)
	c := exec.Command(m.sqlClient, connStr)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return sqlClientErrorMsg{err: fmt.Errorf("%s: %w", m.sqlClient, err)}
		}
		return nil
	})
}

func (m *Model) clearFlashAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return flashMsg{}
	})
}

func (m *Model) clearClipboardAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return clipboardClearMsg{}
	})
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
				i++
			}
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// startDissolve captures the screen, starts async disconnect, and begins the dissolve animation.
func (m *Model) startDissolve() []tea.Cmd {
	plain := stripANSI(m.buildMainView())
	lines := strings.Split(plain, "\n")

	grid := make([][]rune, len(lines))
	var cells [][2]int
	for r, line := range lines {
		grid[r] = []rune(line)
		for c, ch := range grid[r] {
			if ch != ' ' {
				cells = append(cells, [2]int{r, c})
			}
		}
	}

	rand.Shuffle(len(cells), func(i, j int) {
		cells[i], cells[j] = cells[j], cells[i]
	})

	batchSz := len(cells) / 40 // ~40 frames at 35ms ≈ 1.4s
	if batchSz < 1 {
		batchSz = 1
	}

	m.dissolve = &dissolveState{
		grid:    grid,
		cells:   cells,
		batchSz: batchSz,
	}
	m.mode = modeQuitting

	return []tea.Cmd{
		func() tea.Msg {
			m.tunnels.DisconnectAll()
			return shutdownDoneMsg{}
		},
		tea.Tick(35*time.Millisecond, func(time.Time) tea.Msg {
			return dissolveTickMsg{}
		}),
	}
}

// renderDissolve renders the dissolving screen in the accent color.
func (m Model) renderDissolve() string {
	if m.dissolve == nil {
		return ""
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#e94560"))
	var b strings.Builder
	for _, line := range m.dissolve.grid {
		b.WriteString(style.Render(string(line)))
		b.WriteString("\n")
	}
	return b.String()
}

// scheduleHealthCheck schedules the next tunnel health check.
func (m *Model) scheduleHealthCheck() tea.Cmd {
	return tea.Tick(healthCheckInterval, func(time.Time) tea.Msg {
		return tunnelHealthMsg{}
	})
}

// checkTunnelHealth pings connected tunnels and reconnects dead ones.
func (m *Model) checkTunnelHealth() []tea.Cmd {
	var cmds []tea.Cmd
	for i := range m.entries {
		e := &m.entries[i]
		if e.Status != StatusConnected {
			continue
		}
		key := tunnelKey(e)
		if !m.tunnels.IsAlive(key) {
			e.Status = StatusConnecting
			e.Error = ""
			cmds = append(cmds, m.tunnels.Connect(e))
		}
	}
	return cmds
}

// --- View ---

func altView(s string) tea.View {
	v := tea.NewView(s)
	v.AltScreen = true
	return v
}

func (m Model) View() tea.View {
	// Full-screen overlays.
	if m.mode == modeHelp {
		return altView(renderHelp(m.width, m.height, m.sqlClient, m.updateAvailable != nil))
	}
	if m.mode == modeConfig {
		return altView(m.editor.view(m.width))
	}
	if m.mode == modeQuitting {
		return altView(m.renderDissolve())
	}
	return altView(m.buildMainView())
}

func (m Model) buildMainView() string {
	var b strings.Builder

	// Header.
	b.WriteString("\n")
	title := headerStyle.Render(" \U0001f529 DRILLBIT ")
	b.WriteString(title)
	if version != "dev" {
		b.WriteString(dimStyle.Render("  v" + version))
	}
	if m.updateAvailable != nil {
		b.WriteString(updateAvailableStyle.Render(
			fmt.Sprintf("  v%s available! (u to update)", m.updateAvailable.Version)))
	} else {
		b.WriteString(dimStyle.Render("  " + m.tagline))
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
		return b.String()
	}

	if len(m.entries) == 0 && len(m.discErrors) == 0 {
		b.WriteString("  " + dimStyle.Render("No databases found.") + "\n")
		b.WriteString("  " + dimStyle.Render("Check your config: "+DefaultConfigPath()) + "\n")
		return b.String()
	}

	// Stats line.
	connected := 0
	for _, e := range m.entries {
		if e.Status == StatusConnected {
			connected++
		}
	}
	stats := dimStyle.Render(fmt.Sprintf("  %d databases", len(m.entries)))
	if connected > 0 {
		stats += statusConnected.Render(fmt.Sprintf("  %d connected", connected))
	}
	if len(m.filtered) != len(m.entries) {
		stats += dimStyle.Render(fmt.Sprintf("  (%d shown)", len(m.filtered)))
	}
	b.WriteString(stats + "\n\n")

	// Filter bar.
	if m.mode == modeFilter {
		filterDisplay := filterLabelStyle.Render("/") + " " + m.filterText + dimStyle.Render("\u258f")
		b.WriteString("  " + filterDisplay + "\n\n")
	} else if m.filterText != "" {
		filterDisplay := filterLabelStyle.Render("/") + " " + m.filterText
		b.WriteString("  " + filterDisplay + "\n\n")
	}

	// Table.
	b.WriteString(m.renderTable())
	b.WriteString("\n")

	// Show full error for selected entry.
	if e := m.selectedEntry(); e != nil && e.Status == StatusError && e.Error != "" {
		b.WriteString("  " + errorMsgStyle.Render("\u2716 "+e.Error) + "\n\n")
	}

	// Edit form (shown inline below table when in edit mode).
	if m.mode == modeEdit {
		b.WriteString(m.renderEditForm())
		b.WriteString("\n")
	}

	// Flash message.
	if m.flash != "" {
		b.WriteString("  " + m.flash + "\n")
	}

	// Bottom bar: contextual based on mode.
	b.WriteString("\n")
	switch m.mode {
	case modeFilter:
		b.WriteString(m.renderFilterBar())
	case modeCopy:
		b.WriteString(m.renderCopyPrompt())
	case modeConfirmQuit:
		b.WriteString(m.renderQuitPrompt())
	case modeEdit:
		b.WriteString(m.renderEditBar())
	default:
		b.WriteString(m.renderHelpBar())
	}
	b.WriteString("\n")

	return b.String()
}

// colWidths computes responsive column widths based on terminal width.
type colWidths struct {
	env, host, tenant, auto, user, pw, db, port int
}

func (m Model) calcColumns() colWidths {
	w := m.width
	if w < 60 {
		w = 80
	}

	// Start with header label lengths as minimums.
	cw := colWidths{
		env: 3, host: 4, tenant: 6, auto: 4,
		user: 4, pw: 2, db: 2, port: 4,
	}

	// Scan visible entries to find max content width per column.
	for _, idx := range m.filtered {
		e := m.entries[idx]
		cw.env = max(cw.env, len(strings.ToUpper(e.Env)))
		cw.host = max(cw.host, len(e.Host))
		cw.tenant = max(cw.tenant, len(e.Tenant))
		cw.user = max(cw.user, len(e.DBUser))
		cw.db = max(cw.db, len(e.Database))
		cw.port = max(cw.port, len(fmt.Sprintf("%d", e.LocalPort)))
	}

	// Add 2 chars padding per column for breathing room.
	cw.env += 2
	cw.host += 2
	cw.tenant += 2
	cw.user += 2
	cw.db += 2

	// 2-char indent + 1 space between each of 9 columns + 1 space before status.
	overhead := 2 + 9
	contentTotal := cw.env + cw.host + cw.tenant + cw.auto + cw.user + cw.pw + cw.db + cw.port

	// Reserve space for status ("✖ Error: some message..." ≈ 20 chars minimum).
	minStatus := 20
	if contentTotal+overhead+minStatus > w {
		// Shrink elastic columns (host, tenant, user, db) proportionally.
		fixedCols := cw.env + cw.auto + cw.pw + cw.port
		budget := w - overhead - minStatus - fixedCols
		if budget < 16 {
			budget = 16
		}
		elastic := cw.host + cw.tenant + cw.user + cw.db
		if elastic > budget {
			cw.host = max(4, cw.host*budget/elastic)
			cw.tenant = max(4, cw.tenant*budget/elastic)
			cw.user = max(4, cw.user*budget/elastic)
			cw.db = max(4, budget-cw.host-cw.tenant-cw.user)
		}
	}

	return cw
}

// renderTable draws the table with per-row environment coloring.
func (m Model) renderTable() string {
	if len(m.filtered) == 0 {
		return "  " + dimStyle.Render("No matches.") + "\n"
	}

	c := m.calcColumns()

	var b strings.Builder

	// Header row.
	header := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*s %s",
		c.env, "ENV",
		c.host, "HOST",
		c.tenant, "TENANT",
		c.auto, "AUTO",
		c.user, "USER",
		c.pw, "PW",
		c.db, "DB",
		c.port, "PORT",
		"STATUS",
	)
	b.WriteString(colHeaderStyle.Render(header) + "\n")

	// Separator.
	sep := "  " + strings.Repeat("\u2500", min(m.width-4, len(header)))
	b.WriteString(dimStyle.Render(sep) + "\n")

	// Determine visible range for scrolling.
	maxVisible := m.height - 14
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
		status := styledStatus(e.Status)

		auto := " "
		if m.isAutoconnect(&e) {
			auto = "*"
		}

		pw := "-"
		if e.Password != "" {
			pw = "\u2022"
		}

		// Truncate fields that are too long.
		host := truncate(e.Host, c.host)
		tenant := truncate(e.Tenant, c.tenant)
		user := truncate(e.DBUser, c.user)
		db := truncate(e.Database, c.db)

		row := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*d",
			c.env, env,
			c.host, host,
			c.tenant, tenant,
			c.auto, auto,
			c.user, user,
			c.pw, pw,
			c.db, db,
			c.port, e.LocalPort,
		)

		if isSelected {
			b.WriteString(selectedStyle.Render(row) + " " + status + "\n")
		} else {
			style := envRowStyle(e.Env)
			b.WriteString(style.Render(row) + " " + status + "\n")
		}
	}

	// Scroll indicator.
	if len(m.filtered) > maxVisible {
		total := len(m.filtered)
		b.WriteString(dimStyle.Render(fmt.Sprintf("  \u2191\u2193 %d / %d", end-start, total)) + "\n")
	}

	return b.String()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-1] + "\u2026"
}

// renderEditForm renders the inline edit form for the selected entry.
func (m Model) renderEditForm() string {
	e := m.selectedEntry()
	if e == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("  " + headerAccent.Render(fmt.Sprintf("Configure %s/%s", e.Host, e.Tenant)) + "\n\n")

	labels := [3]string{"  User:      ", "  Password:  ", "  Database:  "}
	for i, label := range labels {
		if i == m.editFieldCursor {
			b.WriteString(helpKeyStyle.Render("> ") + dimStyle.Render(label[2:]) + m.editFields[i].View() + "\n")
		} else {
			b.WriteString(dimStyle.Render(label) + m.editFields[i].View() + "\n")
		}
	}

	// Autoconnect toggle.
	check := "[ ]"
	if m.editAutoconnect {
		check = "[*]"
	}
	if m.editFieldCursor == 3 {
		b.WriteString(helpKeyStyle.Render("> ") + dimStyle.Render("Autoconnect: ") + helpKeyStyle.Render(check) + "\n")
	} else {
		b.WriteString(dimStyle.Render("  Autoconnect: "+check) + "\n")
	}

	return b.String()
}

// renderHelpBar renders the compact keybinding bar at the bottom.
func (m Model) renderHelpBar() string {
	keys := []struct{ key, desc string }{
		{"Space", "toggle"},
	}
	if m.sqlClient != "" {
		keys = append(keys, struct{ key, desc string }{"\u23ce", m.sqlClient})
	}
	keys = append(keys, []struct{ key, desc string }{
		{"c", "configure"},
		{"y", "copy"},
		{"/", "filter"},
		{"h", "hosts"},
		{"r", "refresh"},
		{"?", "help"},
		{"Esc", "quit"},
	}...)

	var parts []string
	for _, k := range keys {
		parts = append(parts, helpKeyStyle.Render(k.key)+helpBarStyle.Render(":"+k.desc))
	}
	return "  " + strings.Join(parts, "  ")
}

// renderFilterBar renders the filter mode help bar.
func (m Model) renderFilterBar() string {
	return "  " +
		dimStyle.Render("Type to filter") + "  " +
		helpKeyStyle.Render("Enter") + helpBarStyle.Render(":accept") + "  " +
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":clear")
}

// renderCopyPrompt renders the copy mode prompt in the help bar area.
func (m Model) renderCopyPrompt() string {
	return "  " +
		helpKeyStyle.Render("Copy: ") +
		helpKeyStyle.Render("p") + helpBarStyle.Render(":password") + "  " +
		helpKeyStyle.Render("c") + helpBarStyle.Render(":connstr") + "  " +
		dimStyle.Render("Esc:cancel")
}

// renderEditBar renders the edit mode help bar.
func (m Model) renderEditBar() string {
	return "  " +
		helpKeyStyle.Render("Tab") + helpBarStyle.Render(":next field") + "  " +
		helpKeyStyle.Render("Space") + helpBarStyle.Render(":toggle auto") + "  " +
		helpKeyStyle.Render("Enter") + helpBarStyle.Render(":save") + "  " +
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":cancel")
}

// renderQuitPrompt renders the quit confirmation in the help bar area.
func (m Model) renderQuitPrompt() string {
	count := m.activeConnectionCount()
	msg := fmt.Sprintf("Quit? %d active connection", count)
	if count != 1 {
		msg += "s"
	}
	msg += " will close. "
	return "  " +
		statusConnecting.Render(msg) +
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":confirm") + "  " +
		dimStyle.Render("any other key:cancel")
}
