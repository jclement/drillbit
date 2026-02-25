package main

import (
	"fmt"

	"charm.land/lipgloss/v2"
)

type helpBinding struct {
	key  string
	desc string
}

var helpBindingsBase = []helpBinding{
	{"Space", "Toggle connect / disconnect"},
}

var helpBindingsAfterSQL = []helpBinding{
	{"c", "Configure selected (user, pw, db, auto)"},
	{"y", "Copy to clipboard (then p or c)"},
	{"/", "Filter"},
	{"h", "Manage hosts"},
	{"r", "Refresh — re-discover all hosts"},
	{"j / \u2193", "Move down"},
	{"k / \u2191", "Move up"},
	{"?", "Toggle this help"},
	{"Esc", "Quit (confirms if connected)"},
}

func renderHelp(width, height int, sqlClient string, updateAvailable bool) string {
	bindings := make([]helpBinding, len(helpBindingsBase))
	copy(bindings, helpBindingsBase)
	if sqlClient != "" {
		bindings = append(bindings, helpBinding{"Enter", fmt.Sprintf("Connect / open %s (needs user+pw+db)", sqlClient)})
	} else {
		bindings = append(bindings, helpBinding{"Enter", "Connect"})
	}
	bindings = append(bindings, helpBindingsAfterSQL...)
	if updateAvailable {
		bindings = append(bindings, helpBinding{"u", "Update to latest version"})
	}

	s := helpTitleStyle.Render("Keybindings") + "\n\n"
	for _, b := range bindings {
		key := helpKeyStyle.Render(fmt.Sprintf("  %-12s", b.key))
		s += key + "  " + b.desc + "\n"
	}
	box := helpOverlayStyle.Render(s)

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}
