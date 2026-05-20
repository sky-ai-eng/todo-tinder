#!/bin/sh
# Triage Factory container entrypoint.
#
# Behavior summary:
#   1. Run goose-managed forward migrations, retrying on connection
#      failure so a not-yet-ready Postgres doesn't hard-fail the
#      container before compose/Fly finishes wiring DNS. Idempotent
#      and safe to re-run on every restart.
#   2. exec the binary so tini/the container sees its signals
#      directly (no shell intermediary swallowing SIGTERM).
#
# Local-mode (the default) hits step 1 against the local SQLite file
# — no network races, succeeds immediately. So a plain `docker run`
# boots a working single-tenant TF without any env wiring at all.
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

mkdir -p "$TF_HOME"

# Note: we deliberately do NOT source any .env file inside the
# container. `. "$ENV_FILE"` executes arbitrary shell, so a writable
# .env (via a separate vulnerability or a sloppy bind-mount) becomes
# a code-exec-on-restart path. Both supported deploy modes inject
# env vars directly into the process: docker-compose via the
# `environment:` block, Fly via `flyctl secrets set`. The TF binary
# reads everything from os.Getenv, so there's nothing for the shell
# to source.

# --- 1. Migrations (with bounded retry) ------------------------------------
#
# `triagefactory migrate up` opens the DB (Ping included) before
# invoking goose, so a connection failure surfaces here as a non-zero
# exit. Retrying the whole command — instead of probing connectivity
# separately — handles a few things at once:
#
#   - First boot: pg is reachable but goose tables don't exist yet.
#     A separate "wait" probe based on `migrate status` would
#     mis-classify this as "not ready" and burn the whole timeout.
#   - Restart with schema at head: migrate is a fast no-op.
#   - Truly unreachable DB: we surface the real error from migrate
#     after the retry budget is exhausted, so compose/Fly logs
#     contain the diagnostic instead of a generic "wait timed out".
#
# Goose's forward migrations are idempotent — re-running once pg is
# up is safe.

attempts=30
sleep_s=1
attempt=0
while :; do
    attempt=$((attempt + 1))
    if triagefactory migrate up; then
        break
    fi
    if [ "$attempt" -ge "$attempts" ]; then
        echo "migrate up failed after ${attempts} attempts; giving up." >&2
        exit 1
    fi
    echo "migrate up failed (attempt ${attempt}/${attempts}); retrying in ${sleep_s}s..." >&2
    sleep "$sleep_s"
done

# --- 2. exec the binary ----------------------------------------------------
#
# exec replaces the shell with the Go process so tini's signal
# forwarding lands directly on triagefactory. Without the exec, a
# SIGTERM from compose would hit this script and have to be relayed
# manually — losing the chance for graceful shutdown.

exec triagefactory "$@"
