package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	AssetName string // e.g. "drillbit_1.2.3_darwin_arm64.tar.gz"
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
	wantName := fmt.Sprintf("drillbit_%s_%s_%s.tar.gz",
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

	// Extract the "drillbit" binary from the tar.gz.
	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var binaryData []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Name == "drillbit" || strings.HasSuffix(hdr.Name, "/drillbit") {
			binaryData, err = io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("reading binary: %w", err)
			}
			break
		}
	}

	if binaryData == nil {
		return fmt.Errorf("binary not found in archive")
	}

	// Replace current executable via temp file + atomic rename.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}
	execPath = resolveExecPath(execPath)

	tmpPath := execPath + ".update"
	if err := os.WriteFile(tmpPath, binaryData, 0o755); err != nil {
		return fmt.Errorf("writing temp binary: %w", err)
	}

	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replacing binary: %w", err)
	}

	return nil
}

// resolveExecPath follows symlinks to find the real binary path.
func resolveExecPath(path string) string {
	resolved, err := os.Readlink(path)
	if err != nil {
		return path // not a symlink
	}
	if !strings.HasPrefix(resolved, "/") {
		dir := path[:strings.LastIndex(path, "/")+1]
		resolved = dir + resolved
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
