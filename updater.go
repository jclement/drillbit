package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	githubOwner        = "jclement"
	githubRepo         = "drillbit"
	updateCheckTimeout = 10 * time.Second
	downloadTimeout    = 120 * time.Second
)

// updateInfo holds information about an available update.
type updateInfo struct {
	Version      string // e.g. "1.2.3" (no "v" prefix)
	TagName      string // e.g. "v1.2.3"
	HTMLURL      string // GitHub release page URL
	AssetURL     string // direct download URL for the correct archive
	AssetName    string // e.g. "drillbit_1.2.3_darwin_arm64.zip"
	ChecksumURL  string // URL to checksums.txt
	BundleURL    string // URL to checksums.txt.sigstore.json
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
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	wantName := fmt.Sprintf("drillbit_%s_%s_%s.%s",
		ver, runtime.GOOS, runtime.GOARCH, ext)

	var assetURL, assetName, checksumURL, bundleURL string
	for _, a := range rel.Assets {
		switch {
		case a.Name == wantName:
			assetURL = a.BrowserDownloadURL
			assetName = a.Name
		case a.Name == "checksums.txt":
			checksumURL = a.BrowserDownloadURL
		case a.Name == "checksums.txt.sigstore.json":
			bundleURL = a.BrowserDownloadURL
		}
	}

	if assetURL == "" {
		return nil, fmt.Errorf("no asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	return &updateInfo{
		Version:     ver,
		TagName:     rel.TagName,
		HTMLURL:     rel.HTMLURL,
		AssetURL:    assetURL,
		AssetName:   assetName,
		ChecksumURL: checksumURL,
		BundleURL:   bundleURL,
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

	// Verify the release signature if a sigstore bundle is available.
	if info.BundleURL != "" && info.ChecksumURL != "" {
		checksumData, err := httpGet(client, info.ChecksumURL)
		if err != nil {
			return fmt.Errorf("downloading checksums: %w", err)
		}
		bundleData, err := httpGet(client, info.BundleURL)
		if err != nil {
			return fmt.Errorf("downloading signature bundle: %w", err)
		}
		if err := verifySigstoreBundle(checksumData, bundleData); err != nil {
			return fmt.Errorf("signature verification: %w", err)
		}

		// Download the archive and verify its checksum.
		archiveData, err := httpGet(client, info.AssetURL)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		expectedHash, err := findChecksumForAsset(checksumData, info.AssetName)
		if err != nil {
			return err
		}
		if err := verifyChecksum(archiveData, expectedHash); err != nil {
			return fmt.Errorf("archive integrity: %w", err)
		}
		return installBinary(archiveData, info.AssetName)
	}

	// Fallback for pre-signing releases: no verification.
	archiveData, err := httpGet(client, info.AssetURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return installBinary(archiveData, info.AssetName)
}

// installBinary extracts the binary from the archive and replaces the current executable.
func installBinary(archiveData []byte, assetName string) error {
	var binaryData []byte
	var err error
	if strings.HasSuffix(assetName, ".zip") {
		binaryData, err = extractBinaryFromZip(archiveData)
	} else {
		binaryData, err = extractBinaryFromTarGz(archiveData)
	}
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

	return nil, fmt.Errorf("binary %q not found in zip archive", want)
}

// extractBinaryFromTarGz finds the drillbit binary inside a tar.gz archive.
func extractBinaryFromTarGz(data []byte) ([]byte, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	want := binaryName()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if filepath.Base(hdr.Name) == want {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("binary %q not found in tar.gz archive", want)
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

// httpGet downloads a URL and returns the response body.
func httpGet(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// verifySigstoreBundle verifies that checksumData was signed by our GitHub Actions
// workflow using Sigstore keyless signing. Supports both the new protobuf-based
// Sigstore bundle format and the old cosign bundle format (base64Signature/cert).
func verifySigstoreBundle(checksumData, bundleJSON []byte) error {
	// Try the new Sigstore protobuf bundle format first.
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err == nil {
		return verifyNewBundle(checksumData, &b)
	}

	// Fall back to the old cosign bundle format.
	return verifyOldBundle(checksumData, bundleJSON)
}

// verifyNewBundle verifies using the sigstore-go library (protobuf bundle format).
func verifyNewBundle(checksumData []byte, b *bundle.Bundle) error {
	trustedRoot, err := root.NewLiveTrustedRoot(tuf.DefaultOptions())
	if err != nil {
		return fmt.Errorf("loading trusted root: %w", err)
	}

	verifier, err := verify.NewVerifier(trustedRoot,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return fmt.Errorf("creating verifier: %w", err)
	}

	certID, err := verify.NewShortCertificateIdentity(
		"https://token.actions.githubusercontent.com", "",
		"", "^https://github.com/jclement/drillbit/",
	)
	if err != nil {
		return fmt.Errorf("creating certificate identity: %w", err)
	}

	_, err = verifier.Verify(b,
		verify.NewPolicy(
			verify.WithArtifact(bytes.NewReader(checksumData)),
			verify.WithCertificateIdentity(certID),
		),
	)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	return nil
}

// oldCosignBundle is the legacy cosign sign-blob --bundle format.
type oldCosignBundle struct {
	Base64Signature string         `json:"base64Signature"`
	Cert            string         `json:"cert"`
	RekorBundle     oldRekorBundle `json:"rekorBundle"`
}

type oldRekorBundle struct {
	Payload oldRekorPayload `json:"Payload"`
}

type oldRekorPayload struct {
	IntegratedTime int64 `json:"integratedTime"`
}

// verifyOldBundle verifies the old cosign bundle format (base64Signature + cert).
// It verifies the ECDSA signature and checks the certificate's SAN identity.
func verifyOldBundle(checksumData, bundleJSON []byte) error {
	var ob oldCosignBundle
	if err := json.Unmarshal(bundleJSON, &ob); err != nil {
		return fmt.Errorf("parsing bundle: %w", err)
	}
	if ob.Base64Signature == "" || ob.Cert == "" {
		return fmt.Errorf("bundle missing signature or certificate")
	}

	// Decode the signature.
	sig, err := base64.StdEncoding.DecodeString(ob.Base64Signature)
	if err != nil {
		return fmt.Errorf("decoding signature: %w", err)
	}

	// Decode the certificate (base64 → PEM → X.509).
	certPEM, err := base64.StdEncoding.DecodeString(ob.Cert)
	if err != nil {
		return fmt.Errorf("decoding certificate: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parsing certificate: %w", err)
	}

	// Verify the certificate was issued by Fulcio (via TUF trusted root).
	trustedRoot, err := root.NewLiveTrustedRoot(tuf.DefaultOptions())
	if err != nil {
		return fmt.Errorf("loading trusted root: %w", err)
	}
	certVerified := false
	for _, ca := range trustedRoot.FulcioCertificateAuthorities() {
		if _, err := ca.Verify(cert, time.Unix(ob.RekorBundle.Payload.IntegratedTime, 0)); err == nil {
			certVerified = true
			break
		}
	}
	if !certVerified {
		return fmt.Errorf("certificate not issued by a trusted Fulcio CA")
	}

	// Check the certificate's SAN for our GitHub Actions identity.
	foundIdentity := false
	for _, uri := range cert.URIs {
		if strings.HasPrefix(uri.String(), "https://github.com/jclement/drillbit/") {
			foundIdentity = true
			break
		}
	}
	if !foundIdentity {
		return fmt.Errorf("certificate identity does not match expected GitHub workflow")
	}

	// Verify the ECDSA signature.
	ecPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("certificate public key is not ECDSA")
	}
	digest := sha256.Sum256(checksumData)
	if !ecdsa.VerifyASN1(ecPub, digest[:], sig) {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

// findChecksumForAsset looks up the SHA256 hash for a specific asset in checksums.txt.
func findChecksumForAsset(checksumData []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(checksumData), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == assetName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s", assetName)
}

// verifyChecksum compares data's SHA256 hash against an expected hex digest.
func verifyChecksum(data []byte, expectedHex string) error {
	actual := sha256.Sum256(data)
	actualHex := hex.EncodeToString(actual[:])
	if !strings.EqualFold(actualHex, expectedHex) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHex, actualHex)
	}
	return nil
}
