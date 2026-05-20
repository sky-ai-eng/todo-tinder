package db

// rowScanner is the common Scan surface of *sql.Row and *sql.Rows.
// Used by package-level legacy curator helpers (curator.go) that
// still run raw SQL, predating the store migration. Same shape as
// the sqlite-package helper of the same name.
type rowScanner interface {
	Scan(dest ...any) error
}
