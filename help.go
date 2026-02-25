package main

import "fmt"

var helpBindings = []struct {
	key  string
	desc string
}{
	{"c / Enter", "Toggle connect / disconnect"},
	{"d", "Disconnect selected tunnel"},
	{"x", "Open pgcli (if connected)"},
	{"p", "Copy password to clipboard"},
	{"C", "Copy connection string to clipboard"},
	{"/", "Start filtering (or just type)"},
	{"Esc", "Clear filter / close help"},
	{"r", "Refresh — re-discover all hosts"},
	{"j / \u2193", "Move down"},
	{"k / \u2191", "Move up"},
	{"h / ?", "Toggle this help"},
	{"q", "Quit (closes all tunnels)"},
}

func renderHelp() string {
	s := helpTitleStyle.Render("Keybindings") + "\n\n"
	for _, b := range helpBindings {
		key := helpKeyStyle.Render(fmt.Sprintf("  %-12s", b.key))
		s += key + "  " + b.desc + "\n"
	}
	return helpOverlayStyle.Render(s)
}
