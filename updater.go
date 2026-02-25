package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	githubOwner        = "jclement"
	githubRepo         = "drillbit"
	updateCheckTimeout = 10 * time.Second
	downloadTimeout    = 120 * time.Second
)

// updateInfo holds information about an available update.
type updateInfo struct {
	Version   string // e.g. "1.2.3" (no "v" prefix)
	TagName   string // e.g. "v1.2.3"
	HTMLURL   string // GitHub release page URL
	AssetURL  string // direct download URL for the correct archive
	AssetName string // e.g. "drillbit_1.2.3_darwin_arm64.zip"
}

// --- Bubbletea messages ---

type updateAvailableMsg struct{ info updateInfo }

type updateDoneMsg struct {
	newVersion string
	err        error
}

// --- GitHub API types (minimal) ---

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// binaryName returns the expected binary name for the current platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "drillbit.exe"
	}
	return "drillbit"
}

// checkForUpdate queries GitHub for the latest release.
func checkForUpdate() tea.Cmd {
	return func() tea.Msg {
		if version == "dev" {
			return nil
		}

		info, err := fetchLatestRelease()
		if err != nil || info == nil {
			return nil // fail silently
		}

		if !isNewer(info.Version, version) {
			return nil
		}

		return updateAvailableMsg{info: *info}
	}
}

func fetchLatestRelease() (*updateInfo, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest",
		githubOwner, githubRepo)

	client := &http.Client{Timeout: updateCheckTimeout}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "drillbit/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api: %s", resp.Status)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}

	ver := strings.TrimPrefix(rel.TagName, "v")
	wantName := fmt.Sprintf("drillbit_%s_%s_%s.zip",
		ver, runtime.GOOS, runtime.GOARCH)

	var assetURL, assetName string
	for _, a := range rel.Assets {
		if a.Name == wantName {
			assetURL = a.BrowserDownloadURL
			assetName = a.Name
			break
		}
	}

	if assetURL == "" {
		return nil, fmt.Errorf("no asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	return &updateInfo{
		Version:   ver,
		TagName:   rel.TagName,
		HTMLURL:   rel.HTMLURL,
		AssetURL:  assetURL,
		AssetName: assetName,
	}, nil
}

// performUpdate downloads and installs the new binary.
func performUpdate(info updateInfo) tea.Cmd {
	return func() tea.Msg {
		if err := doUpdate(info); err != nil {
			return updateDoneMsg{err: err}
		}
		return updateDoneMsg{newVersion: info.Version}
	}
}

func doUpdate(info updateInfo) error {
	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Get(info.AssetURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %s", resp.Status)
	}

	// Read entire zip into memory (archive/zip needs random access).
	zipData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	binaryData, err := extractBinaryFromZip(zipData)
	if err != nil {
		return err
	}

	// Replace current executable.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}
	execPath = resolveExecPath(execPath)

	// Write new binary to temp file next to the executable.
	dir := filepath.Dir(execPath)
	tmpFile, err := os.CreateTemp(dir, "drillbit-update-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(binaryData); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp binary: %w", err)
	}
	tmpFile.Close()

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod: %w", err)
	}

	// Rename-away the running binary (works on Windows), then move new one in.
	backupPath := execPath + ".bak"
	os.Remove(backupPath) // clean up any previous backup
	if err := os.Rename(execPath, backupPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("backing up current binary: %w", err)
	}

	if err := os.Rename(tmpPath, execPath); err != nil {
		// Restore from backup.
		os.Rename(backupPath, execPath)
		os.Remove(tmpPath)
		return fmt.Errorf("replacing binary: %w", err)
	}

	// Best-effort cleanup of backup.
	os.Remove(backupPath)

	return nil
}

// extractBinaryFromZip finds the drillbit binary inside a zip archive.
func extractBinaryFromZip(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}

	want := binaryName()
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if name == want {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("zip open %s: %w", f.Name, err)
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}

	return nil, fmt.Errorf("binary %q not found in archive", want)
}

// resolveExecPath follows symlinks to find the real binary path.
func resolveExecPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// isNewer returns true if candidate is a newer semver than current.
func isNewer(candidate, current string) bool {
	candidate = strings.TrimPrefix(candidate, "v")
	current = strings.TrimPrefix(current, "v")

	c := parseSemver(candidate)
	r := parseSemver(current)

	for i := 0; i < 3; i++ {
		if c[i] > r[i] {
			return true
		}
		if c[i] < r[i] {
			return false
		}
	}
	return false
}

func parseSemver(s string) [3]int {
	var parts [3]int
	fmt.Sscanf(s, "%d.%d.%d", &parts[0], &parts[1], &parts[2])
	return parts
}
