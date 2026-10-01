# Building Moombox from Source

This document covers prerequisites and build commands for Moombox on Windows and Linux.

## Prerequisites

- **Go 1.27+** — https://go.dev/dl/ (go.mod carries `toolchain go1.27.1`; any Go ≥ 1.21 auto-downloads it)
- **Node.js 24 LTS** — https://nodejs.org/ (only required for building the BotGuard sidecar tarball; not a runtime dep)
- **FFmpeg** — only required at runtime, not at build time

## One-Time Setup

After a fresh checkout, run these once to populate the `internal/bgutils/embed/` directory with the BotGuard sidecar tarball and the Node.js binaries embedded in the final executable:

```bash
# Build the BotGuard sidecar payload (~4 MB tarball)
cd bgutil-sidecar
npm ci --omit=dev
node build.mjs
cd ..

# Fetch the pinned Node.js binaries for all three platforms
go run ./tools/fetch-node
```

The `tools/fetch-node` step downloads ~150 MB total (Node binaries for Windows x64, Linux x64, and Linux arm64) and gzips them into the embed dir. Idempotent: re-runs are no-ops when the `node-blobs.stamp` it writes beside the blobs matches the pinned versions.

CI runs both steps automatically (see `.github/workflows/release.yml`). For local builds, re-run the first when the Node pin changes and the second whenever anything under `bgutil-sidecar/` changes — `go test ./internal/bgutils/embed/` fails if the tarball no longer matches that directory.

## Build Commands

### Windows x64 (native)

```bash
go build -o Moombox.exe ./cmd/moombox
```

With version info (release builds):
```bash
go build -ldflags "-s -w -X main.version=2.7.0 -X main.commit=$(git rev-parse --short HEAD)" -o Moombox.exe ./cmd/moombox
```

### Linux x64 (cross-compile or native)

```bash
GOOS=linux GOARCH=amd64 go build -o moombox-linux-amd64 ./cmd/moombox
```

### Linux arm64 (cross-compile)

```bash
GOOS=linux GOARCH=arm64 go build -o moombox-linux-arm64 ./cmd/moombox
```

Cross-compiling Linux binaries from a Windows dev box uses the same env vars — Go handles the toolchain transparently because Moombox uses `CGO_ENABLED=0`.

### Docker image

The `Dockerfile` runs the whole pipeline (sidecar payload build, fetch-node, Go cross-compile) inside the image build — no local Go or Node toolchain needed:

```bash
docker build -t moombox .
docker build --platform linux/arm64 -t moombox:arm64 .   # cross-build via buildx
```

The runtime stage is Debian (glibc required by the embedded Node binary) with FFmpeg installed, and an entrypoint that seeds a container-appropriate `config.toml` (`network_access = "lan"`, all state under `/data`) on first run. See `docker-compose.yml` for the recommended run configuration.

## Windows Resource Embedding (optional)

Moombox.exe ships with an embedded icon, manifest, and version info on Windows. CI generates these at build time via `go-winres`. For local Windows builds with the icon:

```bash
go install github.com/tc-hib/go-winres@latest
cd cmd/moombox
go-winres make --arch amd64
cd ../..
go build -o Moombox.exe ./cmd/moombox
```

The generated `.syso` files are gitignored. Skipping this step produces a working `Moombox.exe` without the embedded icon/version info.

## Signing Releases

Release artifacts are signed with Ed25519 via `cmd/sign`. CI runs this with the private key stored as a GitHub Actions secret (`SIGNING_KEY`). The corresponding public key is hex-embedded in `internal/updater/signing.go` and verifies updates at runtime.

To sign a binary locally (for testing):
```bash
SIGNING_KEY=<hex-encoded-private-key> go run ./cmd/sign Moombox.exe
# Produces Moombox.exe.sig (64-byte Ed25519 signature)
```

To generate a new key pair (for setting up a fresh signing chain):
```bash
go run ./cmd/sign -genkey -out keys.txt
# Reads keys.txt, prints the public key, reminds you to delete the file
```

## Tests

```bash
go test ./...                                       # all tests
go test -v ./internal/engine/...                    # one package, verbose
go test -v -run TestParseDash ./internal/engine/... # one test
go vet ./...                                        # static analysis
```

## CI Workflows

- **`.github/workflows/release.yml`** — runs on tag push (`v*`). Runs the test suite on the tagged commit first, then builds and signs all three platforms, creates a GitHub release with the matching assets and a body listing all download links, and publishes the Docker image. Run it by hand ("Run workflow") for a dry run: every step runs and nothing is published (the image is not pushed; the release upload goes to a draft the job deletes). A tag must equal the version in `cmd/moombox/main.go` or the release fails.
- **`.github/workflows/ci.yml`** — runs on every PR/push to `main`, on ubuntu and windows: gofmt, vet, staticcheck, build, the full test suite. Also the test gate release.yml calls.
- **`.github/workflows/vuln-scan.yml`** — weekly, and on manual dispatch: `govulncheck` over the Go module and `npm audit` over both npm projects.
- **`.github/workflows/docker-publish.yml`** — called by release.yml on a tag, or run by manual dispatch. Builds the multi-arch (linux/amd64 + linux/arm64) Docker image and pushes it to `ghcr.io/vampiricwulf/moombox` (release tags → `X.Y.Z`, `X.Y`, `latest`; manual dispatch on `main` → `edge`).

## Profiling

For memory or CPU investigations, set `MOOMBOX_PPROF=1` before launching. Moombox then exposes the standard Go `net/http/pprof` endpoints on `localhost:6060` (loopback-only). Off by default with zero overhead when unset.

```bash
MOOMBOX_PPROF=1 ./moombox-linux-amd64
# In another terminal:
go tool pprof http://localhost:6060/debug/pprof/heap     # live heap
go tool pprof http://localhost:6060/debug/pprof/profile  # 30s CPU profile
```
