# syntax=docker/dockerfile:1

# Moombox container image — three stages:
#
#   1. sidecar  (node)   Builds the BotGuard sidecar payload tarball
#                        (npm ci + build.mjs), exactly as CI does.
#   2. build    (golang) Fetches the pinned embedded Node binaries
#                        (go run ./tools/fetch-node) and cross-compiles
#                        the moombox binary for the target platform.
#   3. runtime  (debian) ffmpeg + CA certs + the binary + an entrypoint
#                        that seeds a Docker-appropriate config.toml.
#
# Stages 1 and 2 run on the build host's native architecture
# ($BUILDPLATFORM) — Go cross-compiles without emulation (CGO_ENABLED=0),
# and the sidecar tarball is platform-agnostic JS. Only the small runtime
# stage runs under emulation for foreign-arch builds.
#
# The runtime base must be glibc (Debian, not Alpine): the sidecar
# extracts an official nodejs.org Linux binary at runtime, and those are
# linked against glibc.
#
#   docker build -t moombox .
#   docker build --platform linux/arm64 -t moombox:arm64 .

# ── Stage 1: BotGuard sidecar payload ────────────────────────────────────
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS sidecar

WORKDIR /src/bgutil-sidecar
COPY bgutil-sidecar/ ./

# Mirrors .github/workflows/release.yml: full install (build.mjs needs the
# esbuild devDependency to bundle vendored ejs; it prunes devDeps itself
# before creating the production tarball). --ignore-scripts for the same
# supply-chain reason as CI.
RUN npm ci --no-audit --no-fund --ignore-scripts \
    && node build.mjs
# Output: /src/internal/bgutils/embed/sidecar.tar.gz

# ── Stage 2: Go build ────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Fetch the pinned embedded Node binaries before copying the full source,
# so the ~150 MB of nodejs.org downloads stay in a layer that only
# invalidates when the pin (tools/fetch-node) or go.mod changes.
COPY tools/ tools/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go run ./tools/fetch-node

COPY . .
COPY --from=sidecar /src/internal/bgutils/embed/sidecar.tar.gz internal/bgutils/embed/

# VERSION/COMMIT default to empty: the binary then reports the version
# hardcoded in cmd/moombox/main.go. CI overrides both from the git tag.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=
ARG COMMIT=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w${VERSION:+ -X main.version=$VERSION}${COMMIT:+ -X main.commit=$COMMIT}" \
    -o /out/moombox ./cmd/moombox

# ── Stage 3: Runtime ─────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/moombox /usr/local/bin/moombox
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/moombox-entrypoint

# /data holds everything: config.toml, database, logs, staging, finished
# downloads, and the sidecar cache (HOME=/data → /data/.cache). Mount a
# volume here. World-writable so `user:` overrides work without a chown.
RUN mkdir -p /data && chmod 777 /data
WORKDIR /data

# HOME=/data keeps the sidecar's one-time ~36 MB extraction on the volume
# instead of re-extracting on every container recreation. MOOMBOX_NO_TUI
# keeps the app headless even when the container is started with a TTY.
ENV HOME=/data \
    MOOMBOX_NO_TUI=1

EXPOSE 774

ENTRYPOINT ["/usr/local/bin/moombox-entrypoint"]
