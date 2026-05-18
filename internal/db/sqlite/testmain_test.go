package sqlite_test

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain runs once per test binary, before any test in this package.
// We seed the keyring mock here so internal/auth's probeKeychain
// (which caches its result in a sync.Once) sees a working backend on
// its first call, regardless of test ordering.
//
// Without this seam, a future test that imports internal/auth and
// triggers a keychain operation before TestSecretStore_SQLite_*
// would cache a real-OS probe result, and any later test calling
// MockInit() would still hit the stale cached "unavailable" verdict.
// TestMain runs strictly first, so we win the cache-population race
// by construction.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}
