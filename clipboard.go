package main

import (
	"fmt"
	"net/url"

	"github.com/atotto/clipboard"
)

// CopyPassword copies the postgres password to the system clipboard.
func CopyPassword(e *Entry) error {
	if e.Password == "" {
		return fmt.Errorf("no password found for %s/%s", e.Host, e.Tenant)
	}
	return clipboard.WriteAll(e.Password)
}

// CopyConnStr copies a full postgresql connection string to the clipboard.
func CopyConnStr(e *Entry) error {
	if e.Password == "" {
		return fmt.Errorf("no password found for %s/%s", e.Host, e.Tenant)
	}
	connStr := fmt.Sprintf("postgresql://postgres:%s@localhost:%d/%s",
		url.QueryEscape(e.Password), e.LocalPort, e.Tenant)
	return clipboard.WriteAll(connStr)
}
