#!/bin/sh
# Triage Factory container entrypoint.
#
# Behavior summary:
#   1. Multi-mode only — wait for the Postgres server named in
#      TF_DATABASE_URL to accept connections. Compose typically
#      starts pg + tf together; this loop covers the first-boot race.
#   2. Run goose-managed forward migrations. Idempotent — safe to run
#      on every container start, including restarts and rollbacks.
#   3. exec the binary so tini/the container sees its signals
#      directly (no shell intermediary swallowing SIGTERM).
#
# Local-mode (the default) skips step 1; the binary's SQLite init
# handles everything else. So a plain `docker run` boots a working
# single-tenant TF without any env wiring at all.
#
# Note on GoTrue keys: the entrypoint deliberately does NOT generate
# the GoTrue RS256 keypair. GoTrue runs in a separate container and
# reads GOTRUE_JWT_KEYS / GOTRUE_JWT_SECRET from its own env; a key
# generated inside THIS container can't reach it. Operators provision
# the keypair once on the host before `docker compose up`:
#
#   triagefactory jwk-init --write-env .env
#
# That writes the values into the compose .env which both services
# interpolate from. Same model for Fly: pass the generated values to
# the GoTrue deployment's secrets, not this image's environment.

set -eu

TF_HOME="${TF_HOME:-/root/.triagefactory}"
ENV_FILE="${TF_ENV_FILE:-$TF_HOME/.env}"

mkdir -p "$TF_HOME"

# Source the env file so any DB / app vars the operator stashed there
# are visible to migrate + the server. Optional — both Fly secrets
# and compose env: blocks set these directly in the process env.
if [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC1090
    set -a; . "$ENV_FILE"; set +a
fi

# --- 1. Wait for Postgres (multi-mode only) --------------------------------
#
# Compose's depends_on/healthcheck handles this in the happy path, but
# Fly Machines + manual restarts don't have that wiring. A bounded
# wait makes container start order forgiving without masking a truly
# unreachable DB — 30s ceiling, 1s polls, then fall through to
# migrate, which will fail loudly with the real connection error.

if [ "${TF_MODE:-local}" = "multi" ] && [ -n "${TF_DATABASE_URL:-}" ]; then
    echo "Waiting for Postgres (up to 30s)..." >&2
    i=0
    until triagefactory migrate status >/dev/null 2>&1; do
        i=$((i + 1))
        if [ "$i" -ge 30 ]; then
            echo "Postgres not reachable after 30s; proceeding anyway (migrate will surface the real error)." >&2
            break
        fi
        sleep 1
    done
fi

# --- 2. Migrations (always) ------------------------------------------------
#
# Goose forward-only migrations are idempotent — re-running is a no-op
# when the schema is already at head. Both modes hit this; local mode
# stamps the SQLite file at ~/.triagefactory/triagefactory.db.

triagefactory migrate up

# --- 3. exec the binary ----------------------------------------------------
#
# exec replaces the shell with the Go process so tini's signal
# forwarding lands directly on triagefactory. Without the exec, a
# SIGTERM from compose would hit this script and have to be relayed
# manually — losing the chance for graceful shutdown.

exec triagefactory "$@"
