# syntax=docker/dockerfile:1.7
#
# Triage Factory runtime image.
#
# Multi-stage:
#   1. frontend-builder — node:20-alpine, builds frontend/dist for go:embed
#   2. sdk-builder      — node:20-alpine, pre-installs the Agent SDK so
#                         EnsureSDK is a no-op at runtime (the runtime layer
#                         has no node)
#   3. go-builder       — golang:1.26-alpine, static (CGO_ENABLED=0) build
#                         of the TF binary with the embedded frontend
#   4. runtime          — alpine:3.20, ships triagefactory + runsc + apk
#                         tools + the pre-installed SDK + entrypoint.sh
#
# Same image targets both deployments:
#   - Fly.io hosted SaaS (fly.toml at repo root)
#   - Self-host docker-compose (docker-compose.yml at repo root)
#
# Multi-arch (amd64 + arm64) via buildx; TARGETARCH picks runsc + the
# runtime alpine variant. The alpine minirootfs the in-container
# sandbox extracts at first launch is selected at RUNTIME by the Go
# binary (internal/sandbox/rootfs.go alpineRootfsForArch via
# runtime.GOARCH), NOT here — this Dockerfile only fetches the right
# runsc.

# --- stage 1: frontend ------------------------------------------------------

FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# --- stage 2: Agent SDK pre-install -----------------------------------------
#
# Materializes ~/.triagefactory/sdk/ exactly as EnsureSDK would on a
# fresh local install: copies the pinned package.json + lockfile +
# wrapper.mjs from internal/agentproc, runs `npm ci`, leaves the
# result for the runtime stage to COPY in. The reorder in
# install.go's EnsureSDK (SDK presence check first, node/npm check
# second) makes the runtime layer see this as already-installed and
# skip both the install AND the node/npm presence checks.

FROM --platform=$BUILDPLATFORM node:20-alpine AS sdk-builder
WORKDIR /sdk-install
# package-lock.json is the source of truth for transitive deps.
# wrapper.mjs is the runtime shim the host binary materializes.
COPY internal/agentproc/package-lock.json ./package-lock.json
COPY internal/agentproc/wrapper.mjs ./wrapper.mjs
# Inline package.json matching what writePackageJSON emits at runtime.
# Keep the sdkVersion in sync with internal/agentproc/install.go's
# sdkVersion constant — a mismatch will cause the runtime check
# (sdkAlreadyInstalled) to re-trigger the install path.
ARG SDK_VERSION=0.2.137
RUN printf '{\n  "name": "triagefactory-sdk-runtime",\n  "private": true,\n  "type": "module",\n  "dependencies": {\n    "@anthropic-ai/claude-agent-sdk": "%s"\n  }\n}\n' "$SDK_VERSION" > package.json && \
    npm ci --no-audit --no-fund --omit=dev

# --- stage 3: static Go binary ----------------------------------------------

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src
# Pre-download deps so the source-copy layer rebuilds fast on edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# embed.go pulls frontend/dist/ into the binary; ship it in before build.
COPY --from=frontend-builder /src/frontend/dist ./frontend/dist
# Static build for alpine (musl) runtime. The same binary is also
# bind-mounted into the gVisor sandbox's alpine rootfs at runtime, so
# musl-static is the only correct shape — a glibc-linked binary would
# fail to exec in either alpine container.
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/triagefactory .

# --- stage 4: runtime -------------------------------------------------------

FROM alpine:3.20 AS runtime

# - iptables / iproute2: sandbox netns + NAT POSTROUTING setup
# - ca-certificates: outbound TLS from the TF process (GitHub, Jira,
#   Anthropic) and from in-sandbox tools (when they reach back to the
#   host for cached rootfs / pre-installed SDK).
# - tini: PID 1 reaper so signal handling (Ctrl-C in interactive runs,
#   compose stop) propagates cleanly to the Go binary.
# - tzdata: timestamps in logs render in operator-local time when TZ is set.
# - curl: healthcheck probes in compose/fly.
RUN apk add --no-cache \
    iptables \
    iproute2 \
    ca-certificates \
    tini \
    tzdata \
    curl

# Pull the per-arch runsc binary. gVisor's release tree publishes both
# x86_64 and aarch64 builds at the same URL prefix; TARGETARCH (set by
# buildx) selects the right one. SHA512 ships alongside; verify before
# install so a tampered CDN fails loudly at image build time.
ARG TARGETARCH
RUN set -eux; \
    case "$TARGETARCH" in \
        amd64) GVISOR_ARCH=x86_64 ;; \
        arm64) GVISOR_ARCH=aarch64 ;; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    base=https://storage.googleapis.com/gvisor/releases/release/latest/${GVISOR_ARCH}; \
    curl -fsSL -o /usr/local/bin/runsc "$base/runsc"; \
    curl -fsSL -o /tmp/runsc.sha512 "$base/runsc.sha512"; \
    (cd /usr/local/bin && sha512sum -c /tmp/runsc.sha512); \
    rm -f /tmp/runsc.sha512; \
    chmod 0755 /usr/local/bin/runsc

# Pre-installed Agent SDK. EnsureSDK reads os.UserHomeDir() → /root
# in the container and matches against this path.
COPY --from=sdk-builder /sdk-install /root/.triagefactory/sdk

# The TF binary itself.
COPY --from=go-builder /out/triagefactory /usr/local/bin/triagefactory

# Entrypoint script: first-boot JWK init, Postgres wait, migrations,
# then exec the binary. Lives in the repo at docker/entrypoint.sh.
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

# Default mode — overridden by Fly secrets or compose env. Local mode
# is the safe default (no Postgres, no GoTrue) so a `docker run`
# without any env vars boots into a working single-tenant binary that
# operators can poke at to validate the image. Multi-mode operators
# set TF_MODE=multi + TF_DATABASE_URL at deploy time.
ENV TF_MODE=local

EXPOSE 3000

# tini reaps zombies + forwards signals to the binary (the entrypoint
# exec's triagefactory after first-boot setup). Using -g so the whole
# process group sees SIGTERM, not just PID 1.
ENTRYPOINT ["/sbin/tini", "-g", "--", "/entrypoint.sh"]
CMD ["--host", "0.0.0.0", "--port", "3000", "--no-browser"]
