#!/bin/sh
# Triage Factory container entrypoint.
#
# Behavior summary:
#   1. Multi-mode only — if no GoTrue keypair exists at
#      /root/.triagefactory/.env, prompt interactively (TTY) or
#      auto-generate when TF_AUTO_JWK_INIT=1 (headless).
#   2. Multi-mode only — wait for the Postgres server named in
#      TF_DATABASE_URL to accept connections. Compose typically
#      starts pg + tf together; this loop covers the first-boot race.
#   3. Run goose-managed forward migrations. Idempotent — safe to run
#      on every container start, including restarts and rollbacks.
#   4. exec the binary so tini/the container sees its signals
#      directly (no shell intermediary swallowing SIGTERM).
#
# Local-mode (the default) skips steps 1 and 2; the binary's SQLite
# init handles everything else. So a plain `docker run` boots a
# working single-tenant TF without any env wiring at all.

set -eu

TF_HOME="${TF_HOME:-/root/.triagefactory}"
ENV_FILE="${TF_ENV_FILE:-$TF_HOME/.env}"

mkdir -p "$TF_HOME"

# Source the env file so JWK + DB vars set by jwk-init or the
# operator's first-boot setup are visible to migrate + the server.
if [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC1090
    set -a; . "$ENV_FILE"; set +a
fi

# --- 1. First-boot JWK init (multi-mode only) ------------------------------

if [ "${TF_MODE:-local}" = "multi" ]; then
    # The keypair lands in $ENV_FILE as GOTRUE_JWT_KEYS + GOTRUE_JWT_SECRET
    # — jwk-init's --write-env target. Presence of GOTRUE_JWT_KEYS in the
    # process env (sourced above) is the signal that init has already run.
    if [ -z "${GOTRUE_JWT_KEYS:-}" ]; then
        if [ -t 0 ] && [ -t 1 ]; then
            printf "GoTrue keypair not found at %s. Generate one now? [y/N] " "$ENV_FILE"
            read -r answer
            case "$answer" in
                y|Y|yes|YES)
                    triagefactory jwk-init --write-env "$ENV_FILE"
                    # shellcheck disable=SC1090
                    set -a; . "$ENV_FILE"; set +a
                    ;;
                *)
                    echo "Aborting; multi-mode requires a keypair." >&2
                    exit 1
                    ;;
            esac
        elif [ "${TF_AUTO_JWK_INIT:-0}" = "1" ]; then
            echo "Auto-generating GoTrue keypair into $ENV_FILE (TF_AUTO_JWK_INIT=1)" >&2
            triagefactory jwk-init --write-env "$ENV_FILE"
            # shellcheck disable=SC1090
            set -a; . "$ENV_FILE"; set +a
        else
            echo "Multi-mode requires a GoTrue keypair, none found at $ENV_FILE." >&2
            echo "Run with -it for interactive setup, or set TF_AUTO_JWK_INIT=1 for headless." >&2
            exit 1
        fi
    fi
fi

# --- 2. Wait for Postgres (multi-mode only) --------------------------------
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

# --- 3. Migrations (always) ------------------------------------------------
#
# Goose forward-only migrations are idempotent — re-running is a no-op
# when the schema is already at head. Both modes hit this; local mode
# stamps the SQLite file at ~/.triagefactory/triagefactory.db.

triagefactory migrate up

# --- 4. exec the binary ----------------------------------------------------
#
# exec replaces the shell with the Go process so tini's signal
# forwarding lands directly on triagefactory. Without the exec, a
# SIGTERM from compose would hit this script and have to be relayed
# manually — losing the chance for graceful shutdown.

exec triagefactory "$@"
