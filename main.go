package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Printf("drillbit %s\n", version)
			os.Exit(0)
		case "--help":
			printUsage()
			os.Exit(0)
		}
	}

	configPath := DefaultConfigPath()
	if len(os.Args) > 1 && (os.Args[1] == "--config" || os.Args[1] == "-c") {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Error: --config requires a path argument")
			os.Exit(1)
		}
		configPath = os.Args[2]
	}

	// First run: scaffold config if it doesn't exist.
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := ScaffoldConfig(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
		fmt.Println("  \U0001f529 DrillBit — first run!")
		fmt.Println()
		fmt.Printf("  Created config file: %s\n", configPath)
		fmt.Println("  Edit it to add your SSH hosts, then run drillbit again.")
		fmt.Println()
		os.Exit(0)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	m := newModel(cfg)
	m.discovering = true

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: drillbit [options]")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -c, --config <path>   Config file (default: ~/.config/drillbit/config.json)")
	fmt.Println("  -v, --version         Show version")
	fmt.Println("      --help            Show this help")
}
