package main

import (
	tea "charm.land/bubbletea/v2"
)

// Minimal stub for config editor - will be reimplemented for YAML later
type configEditor struct{}

func newConfigEditor(cfg *Config) configEditor {
	return configEditor{}
}

func (ed *configEditor) update(msg tea.KeyPressMsg) (bool, bool, tea.Cmd) {
	// For now, just exit on any key
	return true, false, nil
}

func (ed *configEditor) updatePaste(msg tea.Msg) tea.Cmd {
	return nil
}

func (ed *configEditor) view(width int) string {
	return "\nConfig editor temporarily disabled during YAML migration.\nPress any key to return.\n"
}

func (ed *configEditor) toConfig(autoconnect []AutoconnectEntry) *Config {
	return &Config{}
}
