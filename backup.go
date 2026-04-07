package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"
)

// backupFile represents a discovered backup on disk.
type backupFile struct {
	Path      string
	Host      string
	Container string
	Database  string
	Timestamp time.Time
	Size      int64
}

// backupProgressMsg streams backup progress to the UI.
type backupProgressMsg struct {
	bytesWritten int64
	message      string
	done         bool
	err          error
	filePath     string // set on successful completion
	next         tea.Cmd
}

// restoreProgressMsg streams restore progress to the UI.
type restoreProgressMsg struct {
	bytesRead int64
	totalSize int64
	message   string
	phase     string // "drop", "restore", "done"
	done      bool
	err       error
	next      tea.Cmd
}

// backupFileName generates a filename like host--container--db--20260406_153045.sql.gz
// Uses -- as field separator to avoid ambiguity with underscores in names.
func backupFileName(host, container, database string) string {
	ts := time.Now().Format("20060102_150405")
	h := sanitizeFilename(host)
	c := sanitizeFilename(container)
	d := sanitizeFilename(database)
	return fmt.Sprintf("%s--%s--%s--%s.sql.gz", h, c, d, ts)
}

func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// listBackups scans the backup directory and returns all found backups,
// parsed from filenames matching host--container--db--YYYYMMDD_HHMMSS.sql.gz
func listBackups(backupDir string) []backupFile {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil
	}

	var backups []backupFile
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".sql.gz") {
			continue
		}

		base := strings.TrimSuffix(name, ".sql.gz")
		// Format: host--container--db--YYYYMMDD_HHMMSS
		parts := strings.Split(base, "--")
		if len(parts) != 4 {
			continue
		}

		host := parts[0]
		container := parts[1]
		db := parts[2]

		// Timestamp part is YYYYMMDD_HHMMSS
		tsParts := strings.Split(parts[3], "_")
		if len(tsParts) != 2 {
			continue
		}
		ts, err := time.Parse("20060102150405", tsParts[0]+tsParts[1])
		if err != nil {
			continue
		}

		info, _ := de.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}

		backups = append(backups, backupFile{
			Path:      filepath.Join(backupDir, name),
			Host:      host,
			Container: container,
			Database:  db,
			Timestamp: ts,
			Size:      size,
		})
	}

	// Sort newest first.
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Timestamp.After(backups[j].Timestamp)
	})

	return backups
}

// sortBackupsForEntry returns backups sorted: matching host/container/db first
// (newest), then the rest (newest). Returns the separator index.
func sortBackupsForEntry(backups []backupFile, host, container, database string) (sorted []backupFile, separatorIdx int) {
	var matching, others []backupFile
	h := sanitizeFilename(host)
	c := sanitizeFilename(container)
	d := sanitizeFilename(database)
	for _, b := range backups {
		if strings.EqualFold(b.Host, h) && strings.EqualFold(b.Container, c) && strings.EqualFold(b.Database, d) {
			matching = append(matching, b)
		} else {
			others = append(others, b)
		}
	}
	sorted = append(sorted, matching...)
	separatorIdx = len(matching)
	sorted = append(sorted, others...)
	return
}

// nextBackupProgress reads the next progress event from the channel.
func nextBackupProgress(ch <-chan backupProgressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return backupProgressMsg{done: true}
		}
		if !msg.done {
			msg.next = nextBackupProgress(ch)
		}
		return msg
	}
}

// performBackup runs pg_dump inside the remote Docker container via SSH,
// pipes through gzip locally, and writes to disk. This avoids version
// mismatch since the container's pg_dump always matches its server.
// No tunnel needed — we docker exec directly on the remote host.
func performBackup(e *Entry, backupDir string) tea.Cmd {
	ch := make(chan backupProgressMsg, 50)

	go func() {
		defer close(ch)

		if err := os.MkdirAll(backupDir, 0o755); err != nil {
			ch <- backupProgressMsg{err: fmt.Errorf("create backup dir: %w", err), done: true}
			return
		}

		ch <- backupProgressMsg{message: "Connecting to remote host..."}

		client, err := dialSSH(e.SSHHost)
		if err != nil {
			ch <- backupProgressMsg{err: fmt.Errorf("ssh: %w", err), done: true}
			return
		}
		defer client.Close()

		filename := backupFileName(e.Host, e.Container, e.Database)
		outPath := filepath.Join(backupDir, filename)

		outFile, err := os.Create(outPath)
		if err != nil {
			ch <- backupProgressMsg{err: fmt.Errorf("create file: %w", err), done: true}
			return
		}

		var bytesWritten atomic.Int64
		cw := &countingWriter{w: outFile, count: &bytesWritten}
		gzw := gzip.NewWriter(cw)

		ch <- backupProgressMsg{message: "Running pg_dump in container..."}

		docker := dockerCmd(client)

		// Run pg_dump inside the Docker container.
		cmd := fmt.Sprintf("%s exec %s pg_dump -U %s --no-owner --no-acl %s",
			docker,
			shellQuote(e.Container),
			shellQuote(e.DBUser),
			shellQuote(e.Database),
		)

		session, err := client.NewSession()
		if err != nil {
			outFile.Close()
			os.Remove(outPath)
			ch <- backupProgressMsg{err: fmt.Errorf("ssh session: %w", err), done: true}
			return
		}

		stdout, err := session.StdoutPipe()
		if err != nil {
			session.Close()
			outFile.Close()
			os.Remove(outPath)
			ch <- backupProgressMsg{err: fmt.Errorf("stdout pipe: %w", err), done: true}
			return
		}

		if err := session.Start(cmd); err != nil {
			session.Close()
			outFile.Close()
			os.Remove(outPath)
			ch <- backupProgressMsg{err: fmt.Errorf("start pg_dump: %w", err), done: true}
			return
		}

		// Progress ticker goroutine.
		doneCh := make(chan struct{})
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-doneCh:
					return
				case <-ticker.C:
					b := bytesWritten.Load()
					ch <- backupProgressMsg{
						bytesWritten: b,
						message:      fmt.Sprintf("Dumping... %s compressed", formatBytes(b)),
					}
				}
			}
		}()

		// Copy stdout → gzip → counting writer → file.
		_, copyErr := io.Copy(gzw, stdout)
		waitErr := session.Wait()
		close(doneCh)
		session.Close()

		gzErr := gzw.Close()
		fileErr := outFile.Close()

		// Check for errors in priority order.
		if waitErr != nil {
			os.Remove(outPath)
			ch <- backupProgressMsg{err: fmt.Errorf("pg_dump failed: %w", waitErr), done: true}
			return
		}
		if copyErr != nil {
			os.Remove(outPath)
			ch <- backupProgressMsg{err: fmt.Errorf("stream: %w", copyErr), done: true}
			return
		}
		if gzErr != nil {
			os.Remove(outPath)
			ch <- backupProgressMsg{err: fmt.Errorf("gzip close: %w", gzErr), done: true}
			return
		}
		if fileErr != nil {
			ch <- backupProgressMsg{err: fmt.Errorf("file close: %w", fileErr), done: true}
			return
		}

		info, _ := os.Stat(outPath)
		var finalSize int64
		if info != nil {
			finalSize = info.Size()
		}

		ch <- backupProgressMsg{
			bytesWritten: finalSize,
			message:      fmt.Sprintf("Backup complete: %s (%s)", filename, formatBytes(finalSize)),
			done:         true,
			filePath:     outPath,
		}
	}()

	return nextBackupProgress(ch)
}

// nextRestoreProgress reads the next restore progress event.
func nextRestoreProgress(ch <-chan restoreProgressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return restoreProgressMsg{done: true}
		}
		if !msg.done {
			msg.next = nextRestoreProgress(ch)
		}
		return msg
	}
}

// performRestore drops the public schema and restores from a gzipped SQL
// backup by running psql inside the remote Docker container via SSH.
// No tunnel needed — we docker exec directly on the remote host.
func performRestore(e *Entry, backupPath string) tea.Cmd {
	ch := make(chan restoreProgressMsg, 50)

	go func() {
		defer close(ch)

		info, err := os.Stat(backupPath)
		if err != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("stat backup: %w", err), done: true}
			return
		}
		totalSize := info.Size()

		ch <- restoreProgressMsg{phase: "drop", message: "Connecting to remote host..."}

		client, err := dialSSH(e.SSHHost)
		if err != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("ssh: %w", err), done: true}
			return
		}
		defer client.Close()

		// Phase 1: Drop and recreate public schema.
		ch <- restoreProgressMsg{phase: "drop", message: "Dropping public schema..."}

		docker := dockerCmd(client)

		// Drop non-default extensions first — DROP SCHEMA CASCADE removes
		// their objects but leaves the pg_extension record, which causes
		// CREATE EXTENSION IF NOT EXISTS in the dump to be a no-op (types
		// like geometry end up as <unknown>).
		dropExtSQL := `DO $$ DECLARE ext RECORD; BEGIN
FOR ext IN SELECT extname FROM pg_extension WHERE extname != 'plpgsql' LOOP
EXECUTE 'DROP EXTENSION IF EXISTS ' || quote_ident(ext.extname) || ' CASCADE';
END LOOP; END $$;`
		dropExtCmd := fmt.Sprintf("%s exec %s psql -U %s -d %s -c %s",
			docker,
			shellQuote(e.Container),
			shellQuote(e.DBUser),
			shellQuote(e.Database),
			shellQuote(dropExtSQL),
		)
		_ = runSSHCommandSimple(client, dropExtCmd) // best-effort

		dropSQL := "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO public;"
		dropCmd := fmt.Sprintf("%s exec %s psql -U %s -d %s -c %s",
			docker,
			shellQuote(e.Container),
			shellQuote(e.DBUser),
			shellQuote(e.Database),
			shellQuote(dropSQL),
		)

		if err := runSSHCommandSimple(client, dropCmd); err != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("drop schema: %w", err), done: true}
			return
		}

		ch <- restoreProgressMsg{phase: "drop", message: "Schema reset complete"}

		// Phase 2: Stream gzipped SQL into psql via docker exec.
		ch <- restoreProgressMsg{
			phase:     "restore",
			message:   "Restoring from backup...",
			totalSize: totalSize,
		}

		inFile, err := os.Open(backupPath)
		if err != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("open backup: %w", err), done: true}
			return
		}
		defer inFile.Close()

		var bytesRead atomic.Int64
		cr := &countingReader{r: inFile, count: &bytesRead}

		gzr, err := gzip.NewReader(cr)
		if err != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("gzip: %w", err), done: true}
			return
		}
		defer gzr.Close()

		// Run psql inside the Docker container, piping stdin.
		restoreCmd := fmt.Sprintf("%s exec -i %s psql -U %s -d %s --quiet -v ON_ERROR_STOP=0",
			docker,
			shellQuote(e.Container),
			shellQuote(e.DBUser),
			shellQuote(e.Database),
		)

		session, err := client.NewSession()
		if err != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("ssh session: %w", err), done: true}
			return
		}

		stdinPipe, err := session.StdinPipe()
		if err != nil {
			session.Close()
			ch <- restoreProgressMsg{err: fmt.Errorf("stdin pipe: %w", err), done: true}
			return
		}

		if err := session.Start(restoreCmd); err != nil {
			session.Close()
			ch <- restoreProgressMsg{err: fmt.Errorf("start psql: %w", err), done: true}
			return
		}

		// Progress ticker.
		doneCh := make(chan struct{})
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-doneCh:
					return
				case <-ticker.C:
					b := bytesRead.Load()
					pct := int(float64(b) / float64(totalSize) * 100)
					if pct > 100 {
						pct = 100
					}
					ch <- restoreProgressMsg{
						bytesRead: b,
						totalSize: totalSize,
						phase:     "restore",
						message:   fmt.Sprintf("Restoring... %d%% (%s / %s)", pct, formatBytes(b), formatBytes(totalSize)),
					}
				}
			}
		}()

		// Pipe gunzipped data into SSH stdin (→ docker exec → psql).
		_, copyErr := io.Copy(stdinPipe, gzr)
		stdinPipe.Close()
		waitErr := session.Wait()
		close(doneCh)
		session.Close()

		if copyErr != nil {
			ch <- restoreProgressMsg{err: fmt.Errorf("stream: %w", copyErr), done: true}
			return
		}
		if waitErr != nil {
			// psql may exit non-zero on non-fatal SQL warnings; only fail on real errors.
			ch <- restoreProgressMsg{err: fmt.Errorf("psql: %w", waitErr), done: true}
			return
		}

		// Terminate all other connections so apps reconnect with fresh
		// type OID caches (PostGIS types get new OIDs after recreate).
		terminateSQL := fmt.Sprintf(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid != pg_backend_pid()`,
			e.Database,
		)
		terminateCmd := fmt.Sprintf("%s exec %s psql -U %s -d %s -c %s",
			docker,
			shellQuote(e.Container),
			shellQuote(e.DBUser),
			shellQuote(e.Database),
			shellQuote(terminateSQL),
		)
		_ = runSSHCommandSimple(client, terminateCmd)

		ch <- restoreProgressMsg{
			bytesRead: totalSize,
			totalSize: totalSize,
			phase:     "done",
			message:   "Restore complete!",
			done:      true,
		}
	}()

	return nextRestoreProgress(ch)
}

// runSSHCommandSimple runs a command on an SSH client and returns any error.
func runSSHCommandSimple(client *ssh.Client, cmd string) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer session.Close()

	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := session.CombinedOutput(cmd)
		if err != nil {
			ch <- result{fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))}
		} else {
			ch <- result{nil}
		}
	}()

	select {
	case r := <-ch:
		return r.err
	case <-time.After(30 * time.Second):
		session.Close()
		return fmt.Errorf("command timed out after 30s")
	}
}

// countingWriter wraps a writer and counts bytes written.
type countingWriter struct {
	w     io.Writer
	count *atomic.Int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.count.Add(int64(n))
	return n, err
}

// countingReader wraps a reader and counts bytes read.
type countingReader struct {
	r     io.Reader
	count *atomic.Int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.count.Add(int64(n))
	return n, err
}

// verifyGzip checks that a file is valid gzip by reading it to completion.
func verifyGzip(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	if _, err := io.Copy(io.Discard, gr); err != nil {
		return err
	}
	return nil
}

// formatBytes returns a human-readable byte size.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
