package main

import (
	"fmt"
	"math/rand"
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

// Edit field indices.
const (
	editFieldUser     = 0
	editFieldPassword = 1
	editFieldDatabase = 2
)

// viewMode tracks what the UI is showing.
type viewMode int

const (
	modeNormal viewMode = iota
	modeFilter
	modeHelp
	modeCopy
	modeConfirmQuit
	modeEdit     // edit selected entry (user, pw, db)
	modeQuitting // dissolve animation during shutdown
)

// flashMsg is used to clear the status flash after a delay.
type flashMsg struct{}

// clipboardClearMsg is sent to wipe the clipboard after a timeout.
type clipboardClearMsg struct{}

type dissolveTickMsg struct{}
type shutdownDoneMsg struct{}
type spinnerTickMsg struct{}

// Mouse event messages.
type mouseSelectMsg struct{ row int }
type mouseScrollMsg struct{ delta int }

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

	// Edit mode: override table with row selection + inline input.
	editRow      int             // 0=user, 1=password, 2=database
	editInput    textinput.Model // active text input (when editActive)
	editActive   bool            // true when typing into a field
	editDetected [3]string       // snapshot of entry values at edit start

	width  int
	height int

	discovering    bool
	discoverLog    []logEntry // scrolling progress log
	pendingEntries []Entry    // accumulated during discovery
	discErrors     []hostError
	hostsTotal     int // total hosts to scan
	hostsDone      int // completed hosts counter
	dbsFound       int // running database count
	spinnerFrame   int // animation frame counter

	flash         string // ephemeral status message
	tagline       string // random tagline picked at startup
	sqlClient     string // "pgcli", "psql", or "" if neither found
	pendingLaunch string // tunnel key to auto-launch SQL client once connected

	// Update tracking.
	updateAvailable *updateInfo
	updating        bool

	// Mouse tracking for double-click detection.
	lastClickTime time.Time
	lastClickRow  int

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
		discovering: true,
		hostsTotal:  len(cfg.Hosts),
	}
}

// --- Fuzzy matching ---

type entrySource struct {
	entries []Entry
	indices []int
}

func (s entrySource) String(i int) string {
	e := s.entries[s.indices[i]]
	return fmt.Sprintf("%s %s %s", e.Env, e.Host, e.Container)
}

func (s entrySource) Len() int { return len(s.indices) }

// --- Init ---

func (m Model) Init() tea.Cmd {
	m.discovering = true
	return tea.Batch(
		discoverAll(m.cfg),
		checkForUpdate(),
		m.scheduleHealthCheck(),
		m.scheduleSpinnerTick(),
	)
}

// --- Update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case spinnerTickMsg:
		if m.discovering {
			m.spinnerFrame++
			cmds = append(cmds, m.scheduleSpinnerTick())
		}

	case discoverUpdate:
		if msg.done {
			// All hosts finished — finalize.
			m.discovering = false
			AssignPorts(m.pendingEntries)

			// Merge: carry over status from active tunnels on refresh.
			existing := make(map[string]*Entry, len(m.entries))
			for i := range m.entries {
				existing[tunnelKey(&m.entries[i])] = &m.entries[i]
			}
			for i := range m.pendingEntries {
				key := tunnelKey(&m.pendingEntries[i])
				if old, ok := existing[key]; ok {
					m.pendingEntries[i].Status = old.Status
					m.pendingEntries[i].Error = old.Error
					m.pendingEntries[i].ContainerIP = old.ContainerIP
					m.pendingEntries[i].LocalPort = old.LocalPort
				}
			}

			firstRun := len(m.entries) == 0
			m.entries = m.pendingEntries
			m.pendingEntries = nil
			m.applyFilter()

			// Summary flash.
			hosts := m.hostsTotal - len(m.discErrors)
			word := "hosts"
			if hosts == 1 {
				word = "host"
			}
			m.flash = flashStyle.Render(fmt.Sprintf("\u2713 Scan complete \u2014 %d targets across %d %s", m.dbsFound, hosts, word))
			cmds = append(cmds, m.clearFlashAfter(3*time.Second))

			if firstRun {
				cmds = append(cmds, m.autoconnect()...)
			}
		} else {
			// Incremental update.
			if msg.log != nil {
				m.discoverLog = append(m.discoverLog, *msg.log)
			}
			if len(msg.entries) > 0 {
				m.pendingEntries = append(m.pendingEntries, msg.entries...)
				m.dbsFound += len(msg.entries)
			}
			if msg.hostErr != nil {
				m.discErrors = append(m.discErrors, *msg.hostErr)
			}
			if msg.hostDone {
				m.hostsDone++
			}
			if msg.next != nil {
				cmds = append(cmds, msg.next)
			}
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

	case mouseSelectMsg:
		if m.mode == modeNormal && msg.row >= 0 && msg.row < len(m.filtered) {
			now := time.Now()
			if msg.row == m.lastClickRow && now.Sub(m.lastClickTime) < 500*time.Millisecond {
				// Double-click: toggle connection.
				if e := m.selectedEntry(); e != nil {
					cmds = append(cmds, m.toggleTunnel(e)...)
				}
			}
			m.cursor = msg.row
			m.lastClickTime = now
			m.lastClickRow = msg.row
		}

	case mouseScrollMsg:
		if m.mode == modeNormal {
			m.cursor += msg.delta
			if m.cursor < 0 {
				m.cursor = 0
			}
			if m.cursor >= len(m.filtered) {
				m.cursor = len(m.filtered) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
		}

	case tea.PasteMsg:
		if m.mode == modeEdit && m.editActive {
			var cmd tea.Cmd
			m.editInput, cmd = m.editInput.Update(msg)
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
			cmds = append(cmds, m.toggleTunnel(e)...)
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

	case "a":
		if e := m.selectedEntry(); e != nil {
			m.toggleAutoconnect(e)
			label := "off"
			if m.isAutoconnect(e) {
				label = "on"
			}
			if err := SaveConfig(m.cfg, m.configPath); err != nil {
				m.flash = errorMsgStyle.Render(fmt.Sprintf("Save: %v", err))
			} else {
				m.flash = flashStyle.Render(fmt.Sprintf("Autoconnect %s for %s/%s", label, e.Host, e.Container))
			}
			cmds = append(cmds, m.clearFlashAfter(2*time.Second))
		}

	case "u":
		if m.updateAvailable != nil && !m.updating {
			m.updating = true
			m.flash = flashStyle.Render("Downloading update...")
			cmds = append(cmds, performUpdate(*m.updateAvailable))
		}

	case "r", "ctrl+r":
		m.discovering = true
		m.discoverLog = nil
		m.pendingEntries = nil
		m.discErrors = nil
		m.hostsDone = 0
		m.dbsFound = 0
		m.hostsTotal = len(m.cfg.Hosts)
		cmds = append(cmds, discoverAll(m.cfg))
		cmds = append(cmds, m.scheduleSpinnerTick())

	case "?":
		m.mode = modeHelp

	case "/":
		m.mode = modeFilter
	}

	return cmds
}

// startEdit initializes the override table for the selected entry.
func (m *Model) startEdit(e *Entry) {
	m.editRow = 0
	m.editActive = false
	m.editDetected = [3]string{e.DBUser, e.Password, e.Database}
	m.mode = modeEdit
}

func (m *Model) updateEdit(msg tea.KeyPressMsg) []tea.Cmd {
	if m.editActive {
		return m.updateEditInput(msg)
	}
	return m.updateEditBrowse(msg)
}

// updateEditBrowse handles key events when browsing override rows.
func (m *Model) updateEditBrowse(msg tea.KeyPressMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch msg.String() {
	case "ctrl+c", "esc":
		m.mode = modeNormal

	case "j", "down", "tab":
		if m.editRow < 2 {
			m.editRow++
		}

	case "k", "up", "shift+tab":
		if m.editRow > 0 {
			m.editRow--
		}

	case "enter":
		// Open inline text input for this row.
		e := m.selectedEntry()
		if e == nil {
			break
		}
		m.editInput = textinput.New()
		m.editInput.CharLimit = 256
		m.editInput.SetWidth(30)
		// Pre-populate with current override value.
		ov := m.getOverrideField(e, m.editRow)
		m.editInput.SetValue(ov)
		m.editInput.Focus()
		m.editActive = true

	case "d":
		// Clear override for this row.
		e := m.selectedEntry()
		if e == nil {
			break
		}
		m.setOverrideField(e, m.editRow, "")
		if err := SaveConfig(m.cfg, m.configPath); err != nil {
			m.flash = errorMsgStyle.Render(fmt.Sprintf("Save: %v", err))
		} else {
			labels := [3]string{"User", "Password", "Database"}
			m.flash = flashStyle.Render(fmt.Sprintf("%s override cleared", labels[m.editRow]))
		}
		cmds = append(cmds, m.clearFlashAfter(2*time.Second))
	}

	return cmds
}

// updateEditInput handles key events when typing an override value.
func (m *Model) updateEditInput(msg tea.KeyPressMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch msg.String() {
	case "ctrl+c", "esc":
		m.editInput.Blur()
		m.editActive = false

	case "enter":
		e := m.selectedEntry()
		if e == nil {
			m.editActive = false
			break
		}
		val := strings.TrimSpace(m.editInput.Value())
		m.setOverrideField(e, m.editRow, val)
		// Also update live entry value.
		if val != "" {
			switch m.editRow {
			case editFieldUser:
				e.DBUser = val
			case editFieldPassword:
				e.Password = val
			case editFieldDatabase:
				e.Database = val
			}
		}
		if err := SaveConfig(m.cfg, m.configPath); err != nil {
			m.flash = errorMsgStyle.Render(fmt.Sprintf("Save: %v", err))
		} else {
			m.flash = flashStyle.Render("Override saved")
		}
		cmds = append(cmds, m.clearFlashAfter(2*time.Second))
		m.editInput.Blur()
		m.editActive = false

	default:
		var cmd tea.Cmd
		m.editInput, cmd = m.editInput.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return cmds
}

// getOverrideField returns the current override value for a field.
func (m *Model) getOverrideField(e *Entry, field int) string {
	for _, h := range m.cfg.Hosts {
		if h.Name != e.Host {
			continue
		}
		for _, db := range h.Databases {
			if db.Container != e.Container {
				continue
			}
			switch field {
			case editFieldUser:
				return db.User
			case editFieldPassword:
				return db.Password
			case editFieldDatabase:
				return db.Database
			}
		}
	}
	return ""
}

// setOverrideField sets (or clears) one field on a DatabaseOverride, creating it if needed.
func (m *Model) setOverrideField(e *Entry, field int, value string) {
	for i := range m.cfg.Hosts {
		if m.cfg.Hosts[i].Name != e.Host {
			continue
		}
		for j := range m.cfg.Hosts[i].Databases {
			if m.cfg.Hosts[i].Databases[j].Container == e.Container {
				switch field {
				case editFieldUser:
					m.cfg.Hosts[i].Databases[j].User = value
				case editFieldPassword:
					m.cfg.Hosts[i].Databases[j].Password = value
				case editFieldDatabase:
					m.cfg.Hosts[i].Databases[j].Database = value
				}
				return
			}
		}
		// No existing override — create one.
		override := DatabaseOverride{Container: e.Container}
		switch field {
		case editFieldUser:
			override.User = value
		case editFieldPassword:
			override.Password = value
		case editFieldDatabase:
			override.Database = value
		}
		m.cfg.Hosts[i].Databases = append(m.cfg.Hosts[i].Databases, override)
		return
	}
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
	case "y", "enter":
		return m.startDissolve()
	case "esc", "n":
		m.mode = modeNormal
		return nil
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
	for _, ac := range m.cfg.Autoconnect() {
		if ac.Host == e.Host && ac.Container == e.Container {
			return true
		}
	}
	return false
}

func (m *Model) toggleAutoconnect(e *Entry) {
	// Find the matching host and database in the config
	for i := range m.cfg.Hosts {
		if m.cfg.Hosts[i].Name == e.Host {
			for j := range m.cfg.Hosts[i].Databases {
				if m.cfg.Hosts[i].Databases[j].Container == e.Container {
					// Toggle the auto flag
					m.cfg.Hosts[i].Databases[j].Auto = !m.cfg.Hosts[i].Databases[j].Auto
					return
				}
			}
			// Database not in config yet - add it with auto:true
			m.cfg.Hosts[i].Databases = append(m.cfg.Hosts[i].Databases, DatabaseOverride{
				Container: e.Container,
				Auto:      true,
			})
			return
		}
	}
}

// toggleTunnel connects or disconnects the given entry.
func (m *Model) toggleTunnel(e *Entry) []tea.Cmd {
	switch e.Status {
	case StatusReady, StatusError:
		e.Status = StatusConnecting
		return []tea.Cmd{m.tunnels.Connect(e)}
	case StatusConnected:
		e.Status = StatusReady
		return []tea.Cmd{m.tunnels.Disconnect(e)}
	}
	return nil
}

func (m *Model) autoconnect() []tea.Cmd {
	var cmds []tea.Cmd
	for _, ac := range m.cfg.Autoconnect() {
		for i := range m.entries {
			e := &m.entries[i]
			if e.Host == ac.Host && e.Container == ac.Container {
				e.Status = StatusConnecting
				cmds = append(cmds, m.tunnels.Connect(e))
			}
		}
	}
	return cmds
}

func (m *Model) launchSQLClient(e *Entry) tea.Cmd {
	c := exec.Command(m.sqlClient, connString(e))
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

	tun := m.tunnels // capture pointer directly, not the receiver
	return []tea.Cmd{
		func() tea.Msg {
			tun.DisconnectAll()
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

// scheduleSpinnerTick schedules the next spinner animation frame.
func (m *Model) scheduleSpinnerTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// spinnerFrames are the animation frames for the discovery spinner.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) View() tea.View {
	// Full-screen overlays.
	if m.mode == modeHelp {
		return altView(renderHelp(m.width, m.height, m.sqlClient, m.updateAvailable != nil))
	}
	if m.mode == modeEdit {
		return altView(m.renderEditOverlay())
	}
	if m.mode == modeQuitting {
		return altView(m.renderDissolve())
	}

	v := altView(m.buildMainView())
	v.OnMouse = m.handleMouse()
	return v
}

// tableRowOffset is the number of lines from top of buildMainView to the first
// data row. Header(2) + stats(2) + column header(1) + separator(1) = ~6 lines.
// This changes when a filter bar is shown (+2) or discovery errors are shown.
func (m Model) tableRowOffset() int {
	offset := 7 // blank + header + blank + stats + blank + colheader + separator

	// Discovery errors.
	if !m.discovering && len(m.discErrors) > 0 {
		offset += len(m.discErrors) + 1
	}

	// Filter bar.
	if m.filterText != "" || m.mode == modeFilter {
		offset += 2
	}

	return offset
}

// handleMouse returns the OnMouse handler for the main view.
func (m Model) handleMouse() func(tea.MouseMsg) tea.Cmd {
	offset := m.tableRowOffset()

	// Compute scroll start for row mapping.
	maxVisible := m.height - 14
	if maxVisible < 5 {
		maxVisible = 5
	}
	scrollStart := 0
	if m.cursor >= maxVisible {
		scrollStart = m.cursor - maxVisible + 1
	}

	nFiltered := len(m.filtered)

	return func(msg tea.MouseMsg) tea.Cmd {
		mouse := msg.Mouse()

		switch msg.(type) {
		case tea.MouseClickMsg:
			if mouse.Button == tea.MouseLeft {
				row := mouse.Y - offset + scrollStart
				if row < 0 || row >= nFiltered {
					return nil
				}
				return func() tea.Msg { return mouseSelectMsg{row: row} }
			}

		case tea.MouseWheelMsg:
			switch mouse.Button {
			case tea.MouseWheelUp:
				return func() tea.Msg { return mouseScrollMsg{delta: -1} }
			case tea.MouseWheelDown:
				return func() tea.Msg { return mouseScrollMsg{delta: 1} }
			}
		}

		return nil
	}
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

	if m.discovering {
		b.WriteString(m.renderDiscovery())
		return b.String()
	}

	// Show discovery errors (persisted after discovery completes).
	for _, e := range m.discErrors {
		b.WriteString("  " + errorMsgStyle.Render(fmt.Sprintf("\u2716 %s: %s", e.host, e.err)) + "\n")
	}
	if len(m.discErrors) > 0 {
		b.WriteString("\n")
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
	default:
		b.WriteString(m.renderHelpBar())
	}
	b.WriteString("\n")

	return b.String()
}

// colWidths computes responsive column widths based on terminal width.
type colWidths struct {
	env, image, host, container, auto, user, pw, db, port int
	showEnv                                                bool
}

func (m Model) calcColumns() colWidths {
	w := m.width
	if w < 60 {
		w = 80
	}

	// Check if any entry has an env label.
	hasEnv := false
	for _, idx := range m.filtered {
		if m.entries[idx].Env != "" {
			hasEnv = true
			break
		}
	}

	// Start with header label lengths as minimums.
	cw := colWidths{
		image: 5, host: 4, container: 6, auto: 4,
		user: 4, pw: 2, db: 2, port: 4,
		showEnv: hasEnv,
	}
	if hasEnv {
		cw.env = 3
	}

	// Scan visible entries to find max content width per column.
	for _, idx := range m.filtered {
		e := m.entries[idx]
		if hasEnv {
			cw.env = max(cw.env, len(strings.ToUpper(e.Env)))
		}
		cw.image = max(cw.image, len(e.Image)+2) // +2 for badge padding
		cw.host = max(cw.host, len(e.Host))
		cw.container = max(cw.container, len(e.Container))
		cw.user = max(cw.user, len(e.DBUser))
		cw.db = max(cw.db, len(e.Database))
		cw.port = max(cw.port, len(fmt.Sprintf("%d", e.LocalPort)))
	}

	// Add 2 chars padding per column for breathing room.
	if hasEnv {
		cw.env += 2
	}
	cw.host += 2
	cw.container += 2
	cw.user += 2
	cw.db += 2

	// Count columns: 9 without env, 10 with env.
	numCols := 9
	if hasEnv {
		numCols = 10
	}
	// 2-char indent + 1 space between each column + 1 space before status.
	overhead := 2 + numCols
	contentTotal := cw.env + cw.image + cw.host + cw.container + cw.auto + cw.user + cw.pw + cw.db + cw.port

	// Reserve space for status ("✖ Error: some message..." ≈ 20 chars minimum).
	minStatus := 20
	if contentTotal+overhead+minStatus > w {
		// Shrink elastic columns (host, container, user, db) proportionally.
		fixedCols := cw.env + cw.image + cw.auto + cw.pw + cw.port
		budget := w - overhead - minStatus - fixedCols
		if budget < 16 {
			budget = 16
		}
		elastic := cw.host + cw.container + cw.user + cw.db
		if elastic > budget {
			cw.host = max(4, cw.host*budget/elastic)
			cw.container = max(4, cw.container*budget/elastic)
			cw.user = max(4, cw.user*budget/elastic)
			cw.db = max(4, budget-cw.host-cw.container-cw.user)
		}
	}

	return cw
}

// renderDiscovery renders the hacker-movie scrolling discovery log.
func (m Model) renderDiscovery() string {
	var b strings.Builder

	// Stats line with spinner.
	spinner := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	stats := fmt.Sprintf("Scanning %d hosts", m.hostsTotal)
	if m.hostsDone > 0 {
		stats += fmt.Sprintf(" \u00b7 %d/%d complete", m.hostsDone, m.hostsTotal)
	}
	if m.dbsFound > 0 {
		stats += fmt.Sprintf(" \u00b7 %d databases found", m.dbsFound)
	}
	b.WriteString("  " + statusConnecting.Render(spinner) + " " + headerAccent.Render(stats) + "\n\n")

	// Scrolling log — show as many lines as fit.
	maxLines := m.height - 10
	if maxLines < 3 {
		maxLines = 3
	}
	start := 0
	if len(m.discoverLog) > maxLines {
		start = len(m.discoverLog) - maxLines
	}
	for i := start; i < len(m.discoverLog); i++ {
		b.WriteString(renderLogEntry(m.discoverLog[i]) + "\n")
	}

	return b.String()
}

// renderTable draws the table with per-row environment coloring.
func (m Model) renderTable() string {
	if len(m.filtered) == 0 {
		return "  " + dimStyle.Render("No matches.") + "\n"
	}

	c := m.calcColumns()

	var b strings.Builder

	// Header row.
	var header string
	if c.showEnv {
		header = fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*s %s",
			c.env, "ENV",
			c.host, "HOST",
			c.container, "CONTAINER",
			c.image, "IMAGE",
			c.auto, "AUTO",
			c.user, "USER",
			c.pw, "PW",
			c.db, "DB",
			c.port, "PORT",
			"STATUS",
		)
	} else {
		header = fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*s %s",
			c.host, "HOST",
			c.container, "CONTAINER",
			c.image, "IMAGE",
			c.auto, "AUTO",
			c.user, "USER",
			c.pw, "PW",
			c.db, "DB",
			c.port, "PORT",
			"STATUS",
		)
	}
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
		container := truncate(e.Container, c.container)
		user := truncate(e.DBUser, c.user)
		db := truncate(e.Database, c.db)

		// Create color-coded badge for image type
		imageBadge := imageTypeBadge(e.Image, c.image)

		var row string
		if c.showEnv {
			env := strings.ToUpper(e.Env)
			row = fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*d",
				c.env, env,
				c.host, host,
				c.container, container,
				c.image, imageBadge,
				c.auto, auto,
				c.user, user,
				c.pw, pw,
				c.db, db,
				c.port, e.LocalPort,
			)
		} else {
			row = fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s %-*s %-*d",
				c.host, host,
				c.container, container,
				c.image, imageBadge,
				c.auto, auto,
				c.user, user,
				c.pw, pw,
				c.db, db,
				c.port, e.LocalPort,
			)
		}

		if isSelected {
			full := row + " " + status
			// Pad to terminal width so the selection highlight covers the full row.
			if pad := m.width - lipgloss.Width(full); pad > 0 {
				full += strings.Repeat(" ", pad)
			}
			b.WriteString(selectedStyle.Render(full) + "\n")
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

// renderEditOverlay renders the override editor as a centered popup.
func (m Model) renderEditOverlay() string {
	e := m.selectedEntry()
	if e == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString(headerAccent.Render(fmt.Sprintf("Override %s/%s", e.Host, e.Container)) + " " + dimStyle.Render("("+e.Image+")") + "\n\n")

	// Column widths.
	const fieldW = 12
	const valW = 20

	// Header.
	hdr := fmt.Sprintf("%-*s %-*s %-*s", fieldW, "FIELD", valW, "DETECTED", valW, "OVERRIDE")
	b.WriteString(colHeaderStyle.Render(hdr) + "\n")
	b.WriteString(dimStyle.Render(strings.Repeat("\u2500", fieldW+valW+valW+4)) + "\n")

	labels := [3]string{"User", "Password", "Database"}
	detected := [3]string{m.editDetected[0], m.editDetected[1], m.editDetected[2]}
	overrides := [3]string{
		m.getOverrideField(e, 0),
		m.getOverrideField(e, 1),
		m.getOverrideField(e, 2),
	}

	for i := 0; i < 3; i++ {
		cursor := "  "
		if i == m.editRow {
			cursor = helpKeyStyle.Render("\u25b6 ")
		}

		det := detected[i]
		if i == editFieldPassword && det != "" {
			det = "\u2022\u2022\u2022\u2022\u2022\u2022" // mask password
		}

		ov := overrides[i]
		if ov == "" {
			ov = dimStyle.Render("(none)")
		} else if i == editFieldPassword {
			ov = "\u2022\u2022\u2022\u2022\u2022\u2022" // mask password override too
		}

		// When actively editing this row, replace override cell with text input.
		if m.editActive && i == m.editRow {
			ov = m.editInput.View()
		}

		row := fmt.Sprintf("%s%-*s %-*s %s", cursor, fieldW, labels[i], valW, det, ov)
		b.WriteString(row + "\n")
	}

	// Help bar inside the popup.
	b.WriteString("\n")
	b.WriteString(m.renderEditBar())

	box := helpOverlayStyle.Render(b.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
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
		{"c", "override"},
		{"a", "auto"},
		{"y", "copy"},
		{"/", "filter"},
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
	if m.editActive {
		return "  " +
			helpKeyStyle.Render("Enter") + helpBarStyle.Render(":save") + "  " +
			helpKeyStyle.Render("Esc") + helpBarStyle.Render(":cancel")
	}
	return "  " +
		helpKeyStyle.Render("\u2191\u2193") + helpBarStyle.Render(":select") + "  " +
		helpKeyStyle.Render("Enter") + helpBarStyle.Render(":edit") + "  " +
		helpKeyStyle.Render("d") + helpBarStyle.Render(":clear") + "  " +
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":done")
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
		helpKeyStyle.Render("y") + helpBarStyle.Render(":quit") + "  " +
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":cancel")
}
