package db

import (
	"fmt"
	"net/url"
)

// RewriteDSNCreds swaps the userinfo (user:password) component of a
// URL-form Postgres DSN. Used by multi-mode boot to derive the
// authenticator app-pool DSN from the admin DSN: the supabase image
// gives admin + postgres a shared password, but the authenticator role's
// password is set separately by the postgres-postinit sidecar, so the
// two pools share host/db/options but differ in userinfo.
//
// Accepts URL-form DSNs (postgres://user:pass@host/db?...) only. pgx
// also accepts libpq keyword=value strings, but main.go's multi-mode
// branch constructs DSNs from the URL-form TF_DATABASE_URL env, so the
// URL parser covers every caller. Returns an error on a malformed
// DSN or one that doesn't parse as a URL.
func RewriteDSNCreds(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("dsn missing scheme or host (not URL form?)")
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
