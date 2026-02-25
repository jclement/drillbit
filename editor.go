package main

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type editorMode int

const (
	editorList editorMode = iota
	editorAdd
)

type editorHost struct {
	env  string
	host HostConfig
}

type configEditor struct {
	hosts       []editorHost
	cursor      int
	mode        editorMode
	fields      [4]textinput.Model
	fieldCursor int
}

func newConfigEditor(cfg *Config) configEditor {
	var hosts []editorHost
	for env, hcs := range cfg.Environments {
		for _, hc := range hcs {
			hosts = append(hosts, editorHost{env: env, host: hc})
		}
	}
	sortEditorHosts(hosts)

	var fields [4]textinput.Model

	fields[0] = textinput.New()
	fields[0].Placeholder = "e.g. prod, test, staging"
	fields[0].CharLimit = 20

	fields[1] = textinput.New()
	fields[1].Placeholder = "SSH host alias or hostname"
	fields[1].CharLimit = 64

	fields[2] = textinput.New()
	fields[2].Placeholder = "(optional)"
	fields[2].CharLimit = 32

	fields[3] = textinput.New()
	fields[3].Placeholder = "/docker"
	fields[3].CharLimit = 128

	return configEditor{
		hosts:  hosts,
		fields: fields,
	}
}

func sortEditorHosts(hosts []editorHost) {
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].env != hosts[j].env {
			return hosts[i].env < hosts[j].env
		}
		return hosts[i].host.Host < hosts[j].host.Host
	})
}

// toConfig builds a Config from the editor state, preserving autoconnect.
func (ed *configEditor) toConfig(autoconnect []AutoconnectEntry) *Config {
	cfg := &Config{
		Environments: make(map[string][]HostConfig),
		Autoconnect:  autoconnect,
	}
	for _, h := range ed.hosts {
		cfg.Environments[h.env] = append(cfg.Environments[h.env], h.host)
	}
	return cfg
}

// update handles key events. Returns (done, save, cmd).
func (ed *configEditor) update(msg tea.KeyPressMsg) (bool, bool, tea.Cmd) {
	// Ctrl+C always cancels without saving.
	if msg.String() == "ctrl+c" {
		if ed.mode == editorAdd {
			ed.fields[ed.fieldCursor].Blur()
		}
		return true, false, nil
	}

	switch ed.mode {
	case editorList:
		if ed.updateList(msg) {
			return true, true, nil // Esc = save & close
		}
	case editorAdd:
		cmd := ed.updateAdd(msg)
		return false, false, cmd
	}
	return false, false, nil
}

func (ed *configEditor) updateList(msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "esc":
		return true
	case "up", "k":
		if ed.cursor > 0 {
			ed.cursor--
		}
	case "down", "j":
		if ed.cursor < len(ed.hosts)-1 {
			ed.cursor++
		}
	case "a":
		ed.mode = editorAdd
		ed.fieldCursor = 0
		for i := range ed.fields {
			ed.fields[i].SetValue("")
			ed.fields[i].Blur()
		}
		ed.fields[0].Focus()
	case "d", "backspace":
		if len(ed.hosts) > 0 && ed.cursor < len(ed.hosts) {
			ed.hosts = append(ed.hosts[:ed.cursor], ed.hosts[ed.cursor+1:]...)
			if ed.cursor >= len(ed.hosts) && ed.cursor > 0 {
				ed.cursor--
			}
		}
	}
	return false
}

func (ed *configEditor) updateAdd(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		ed.mode = editorList
		ed.fields[ed.fieldCursor].Blur()
	case "enter":
		env := strings.TrimSpace(ed.fields[0].Value())
		host := strings.TrimSpace(ed.fields[1].Value())
		if env == "" || host == "" {
			return nil // need at least env and host
		}
		user := strings.TrimSpace(ed.fields[2].Value())
		root := strings.TrimSpace(ed.fields[3].Value())
		if root == "" {
			root = defaultRoot
		}
		ed.hosts = append(ed.hosts, editorHost{
			env:  env,
			host: HostConfig{Host: host, User: user, Root: root},
		})
		sortEditorHosts(ed.hosts)
		for i, h := range ed.hosts {
			if h.host.Host == host && h.env == env {
				ed.cursor = i
				break
			}
		}
		ed.mode = editorList
		ed.fields[ed.fieldCursor].Blur()
	case "tab":
		ed.fields[ed.fieldCursor].Blur()
		ed.fieldCursor = (ed.fieldCursor + 1) % 4
		ed.fields[ed.fieldCursor].Focus()
	case "shift+tab":
		ed.fields[ed.fieldCursor].Blur()
		ed.fieldCursor = (ed.fieldCursor + 3) % 4
		ed.fields[ed.fieldCursor].Focus()
	default:
		var cmd tea.Cmd
		ed.fields[ed.fieldCursor], cmd = ed.fields[ed.fieldCursor].Update(msg)
		return cmd
	}
	return nil
}

func (ed *configEditor) view(width int) string {
	switch ed.mode {
	case editorAdd:
		return ed.viewAdd()
	default:
		return ed.viewList(width)
	}
}

func (ed *configEditor) viewList(width int) string {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(headerStyle.Render(" \U0001f529 DRILLBIT ") + dimStyle.Render("  Configure Hosts"))
	b.WriteString("\n\n")

	header := fmt.Sprintf("  %-10s %-20s %-15s %s", "ENV", "HOST", "USER", "ROOT")
	b.WriteString(colHeaderStyle.Render(header) + "\n")
	sep := "  " + strings.Repeat("\u2500", min(width-4, len(header)))
	b.WriteString(dimStyle.Render(sep) + "\n")

	if len(ed.hosts) == 0 {
		b.WriteString("  " + dimStyle.Render("No hosts configured. Press a to add.") + "\n")
	}

	for i, h := range ed.hosts {
		user := h.host.User
		if user == "" {
			user = "-"
		}
		row := fmt.Sprintf("  %-10s %-20s %-15s %s",
			h.env, h.host.Host, user, h.host.Root)
		if i == ed.cursor {
			b.WriteString(selectedStyle.Render(row) + "\n")
		} else {
			b.WriteString(envRowStyle(h.env).Render(row) + "\n")
		}
	}

	b.WriteString("\n")
	parts := []string{
		helpKeyStyle.Render("a") + helpBarStyle.Render(":add host"),
		helpKeyStyle.Render("d") + helpBarStyle.Render(":remove"),
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":save & close"),
		dimStyle.Render("Ctrl+C:discard"),
	}
	b.WriteString("  " + strings.Join(parts, "  "))
	b.WriteString("\n")

	return b.String()
}

func (ed *configEditor) viewAdd() string {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(headerStyle.Render(" \U0001f529 DRILLBIT ") + dimStyle.Render("  Add Host"))
	b.WriteString("\n\n")

	labels := [4]string{"  Environment:  ", "  Host:         ", "  SSH User:     ", "  Root path:    "}
	for i, label := range labels {
		if i == ed.fieldCursor {
			b.WriteString(helpKeyStyle.Render("> ") + dimStyle.Render(label[2:]) + ed.fields[i].View() + "\n")
		} else {
			b.WriteString(dimStyle.Render(label) + ed.fields[i].View() + "\n")
		}
	}

	b.WriteString("\n")
	parts := []string{
		helpKeyStyle.Render("Tab") + helpBarStyle.Render(":next field"),
		helpKeyStyle.Render("Enter") + helpBarStyle.Render(":add"),
		helpKeyStyle.Render("Esc") + helpBarStyle.Render(":cancel"),
	}
	b.WriteString("  " + strings.Join(parts, "  "))
	b.WriteString("\n")

	return b.String()
}
