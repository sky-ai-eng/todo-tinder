package db

import (
	"net/url"
	"strings"
	"testing"
)

func TestRewriteDSNCreds_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		user     string
		password string
		wantUser string
		wantPass string
	}{
		{
			name:     "basic url",
			dsn:      "postgres://postgres:secret@postgres:5432/postgres",
			user:     "authenticator",
			password: "newpw",
			wantUser: "authenticator",
			wantPass: "newpw",
		},
		{
			name:     "with options",
			dsn:      "postgres://postgres:secret@postgres:5432/postgres?sslmode=disable&search_path=auth",
			user:     "authenticator",
			password: "newpw",
			wantUser: "authenticator",
			wantPass: "newpw",
		},
		{
			name:     "no existing password",
			dsn:      "postgres://postgres@postgres:5432/postgres",
			user:     "authenticator",
			password: "newpw",
			wantUser: "authenticator",
			wantPass: "newpw",
		},
		{
			name:     "password contains url-reserved chars",
			dsn:      "postgres://postgres:secret@postgres:5432/postgres",
			user:     "authenticator",
			password: "p@ss/w?rd#",
			wantUser: "authenticator",
			wantPass: "p@ss/w?rd#",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteDSNCreds(tc.dsn, tc.user, tc.password)
			if err != nil {
				t.Fatalf("RewriteDSNCreds: %v", err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			if u.User.Username() != tc.wantUser {
				t.Errorf("user = %q, want %q", u.User.Username(), tc.wantUser)
			}
			gotPass, _ := u.User.Password()
			if gotPass != tc.wantPass {
				t.Errorf("password = %q, want %q", gotPass, tc.wantPass)
			}
			// Host/path/query must round-trip unchanged.
			orig, _ := url.Parse(tc.dsn)
			if u.Host != orig.Host {
				t.Errorf("host mismatch: %q vs %q", u.Host, orig.Host)
			}
			if u.Path != orig.Path {
				t.Errorf("path mismatch: %q vs %q", u.Path, orig.Path)
			}
			if u.RawQuery != orig.RawQuery {
				t.Errorf("query mismatch: %q vs %q", u.RawQuery, orig.RawQuery)
			}
		})
	}
}

func TestRewriteDSNCreds_Errors(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"empty", ""},
		{"no scheme", "postgres:5432/postgres"},
		{"keyword form", "host=postgres user=postgres password=secret"},
		{"non-postgres scheme", "mysql://root:pw@host:3306/db"},
		{"http scheme", "http://host:5432/postgres"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RewriteDSNCreds(tc.dsn, "authenticator", "pw")
			if err == nil {
				t.Errorf("want error on dsn=%q, got nil", tc.dsn)
			} else if !strings.Contains(err.Error(), "dsn") && !strings.Contains(err.Error(), "parse") && !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error %q lacks expected context", err)
			}
		})
	}
}

func TestRewriteDSNCreds_EmptyUser(t *testing.T) {
	_, err := RewriteDSNCreds("postgres://postgres:secret@postgres:5432/postgres", "", "pw")
	if err == nil {
		t.Fatalf("want error on empty user, got nil")
	}
	if !strings.Contains(err.Error(), "user") {
		t.Errorf("error %q lacks expected context", err)
	}
}

func TestRewriteDSNCreds_PostgresqlScheme(t *testing.T) {
	// The `postgresql://` scheme is the long-form synonym for
	// `postgres://`; both should be accepted.
	got, err := RewriteDSNCreds("postgresql://postgres:secret@postgres:5432/postgres", "authenticator", "pw")
	if err != nil {
		t.Fatalf("RewriteDSNCreds: %v", err)
	}
	if !strings.HasPrefix(got, "postgresql://authenticator:pw@") {
		t.Errorf("unexpected output: %q", got)
	}
}
