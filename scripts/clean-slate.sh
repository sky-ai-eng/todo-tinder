#!/usr/bin/env bash
# Wipe all local state: database, config, keychain credentials.
# Use this to test the first-run experience from scratch.

set -euo pipefail

echo "Cleaning Triage Factory local state..."

# Database
rm -f ~/.triagefactory/triagefactory.db ~/.triagefactory/triagefactory.db-wal ~/.triagefactory/triagefactory.db-shm
echo "  removed database"

# Config
rm -f ~/.triagefactory/config.yaml
echo "  removed config"

# Keychain
for key in github_url github_pat github_username jira_url jira_pat; do
  security delete-generic-password -s triagefactory -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
