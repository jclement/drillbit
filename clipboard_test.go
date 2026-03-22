package main

import (
	"testing"
)

func TestConnString(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{
			name: "basic",
			entry: Entry{
				DBUser:    "postgres",
				Password:  "secret",
				LocalPort: 15432,
				Database:  "mydb",
			},
			want: "postgresql://postgres:secret@localhost:15432/mydb",
		},
		{
			name: "special characters in password",
			entry: Entry{
				DBUser:    "admin",
				Password:  "p@ss/w0rd&foo=bar",
				LocalPort: 10000,
				Database:  "testdb",
			},
			want: "postgresql://admin:p%40ss%2Fw0rd%26foo%3Dbar@localhost:10000/testdb",
		},
		{
			name: "special characters in user",
			entry: Entry{
				DBUser:    "user@domain",
				Password:  "pass",
				LocalPort: 20000,
				Database:  "db",
			},
			want: "postgresql://user%40domain:pass@localhost:20000/db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connString(&tt.entry); got != tt.want {
				t.Errorf("connString() = %q, want %q", got, tt.want)
			}
		})
	}
}
