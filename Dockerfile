# Atomic Site Dockerfile (self-contained).
#
# Builds entirely from the atomicsite/ directory. The CookieProof widget
# bundle is vendored at internal/builder/assets/cookieproof.embed.esm.js
# and embedded into the Go binary via go:embed (see widget_embed.go), so
# Atomic Site has zero runtime or build-time dependency on the sibling
# CookieProof source. To refresh the widget bundle from source, run a
# CookieProof build separately and commit the regenerated asset.
#
# Local build:
#   cd atomicsite && docker build -f Dockerfile -t atomicsite .

# Stage 1: SvelteKit admin SPA.
# Pinned by digest, not by the floating `1-alpine` tag, so a rebuild cannot
# silently pick up a different bun. This must be the multi-arch INDEX digest
# from `docker buildx imagetools inspect oven/bun:1-alpine`, not a per-platform
# manifest digest: pinning a platform manifest forces every build to that one
# architecture. Pinned 2026-07-29 = bun 1.3.14.
FROM oven/bun:1-alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0 AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/bun.lock ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile
COPY frontend/ .
RUN bun run build

# Stage 2: Go server. The frontend build is copied in for go:embed.
#
# CGO note: the analyticsdb package uses marcboeker/go-duckdb/v2 which
# requires CGO + libduckdb. duckdb-go-bindings 0.1.21+ links against
# `backtrace`, `backtrace_symbols`, and `malloc_trim` — backtrace ships
# in glibc (and on alpine via libexecinfo, removed in alpine 3.17+) and
# malloc_trim is glibc-only. Building against musl fails to link, so
# this stage uses the debian-based golang image. The runtime stage
# stays alpine; the binary is statically linked (CGO is wrapped in a
# musl-compatible-via-glibc-static build via -ldflags + go's default
# external linker pulling in libduckdb statically). We force pure
# external linking with -extldflags '-static' so the produced binary
# can run on alpine 3.20 without dragging glibc.
FROM golang:1.26.6-bookworm AS backend
WORKDIR /app
RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
       git ca-certificates build-essential gcc g++ \
    && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
COPY --from=frontend /app/frontend/build ./cmd/server/frontend/build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /server ./cmd/server

# Stage 3: Production image.
#
# Runtime is debian-slim to match the glibc-linked binary from the build
# stage (DuckDB needs glibc symbols absent on musl, see backend stage
# comment). chromium + ca-certificates + tzdata + the chromium font
# stack are required for the headless screenshot tool. bun is copied
# from the official image so the per-site Astro build can run.
FROM debian:bookworm-slim
RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
       ca-certificates tzdata \
       chromium chromium-sandbox \
       fonts-liberation fonts-dejavu-core \
       libnss3 libfreetype6 libharfbuzz0b \
       libstdc++6 libgcc-s1 \
       sqlite3 \
       unzip curl wget \
    && rm -rf /var/lib/apt/lists/*

# Phase 30.4: Litestream for continuous SQLite WAL replication to
# Hetzner Object Storage (or any S3-compatible target). Sub-1-second
# RPO without leaving the data layer. Binary download is the upstream
# canonical path (no Debian package). The binary is statically linked,
# ~25MB, no runtime deps.
#
# Pinned at 0.5.16 (2026-08-05). It was 0.3.13, which is built with
# go1.21.3 and carries 63 open Go stdlib advisories, including
# crypto/x509 name-constraint and crypto/tls ones, in the binary that
# replicates the SQLite WAL to object storage. That is what security-trivy
# has been failing on. 0.3.14 does NOT fix it: it is still go1.21 and
# clears only a handful of the 63. 0.5.16 is built with go1.25.12.
#
# 0.5 is a format change (LTX with compaction levels, not WAL segments), so
# a replica written by 0.3 is not readable by 0.5. That is safe HERE and only
# here: LITESTREAM_REPLICA_URL is unset on the live deployment, verified
# 2026-08-11, so nothing has ever been replicated and there is no 0.3-format
# replica to orphan. If replication is turned on before this ships, restore
# from the old replica FIRST with a 0.3 binary.
#
# litestream.yml was ported in the same commit. 0.5 renamed the keys and the
# parser is NOT strict, so the old `snapshot-interval` / `retention-check-interval`
# / `validation-interval` would have been accepted and silently ignored: a
# declared retention policy that nothing enforces. The ported file was checked
# by running 0.5.16 against a local file replica and reading back the
# "compaction monitor level=9 interval=6h" and "validation monitor interval=1h"
# lines it actually starts.
#
# The download is checksum-gated against upstream's published checksums.txt
# for this tag, verified by hand on 2026-08-11. Any later substitution,
# re-upload or MITM fails the build instead of silently installing a
# different binary as root. To bump: download the new .deb, verify it against
# the release's checksums.txt, then update both hashes.
#
# NOTE the asset naming changed between the two lines: 0.3.x published
# "litestream-v<ver>-linux-amd64.deb", 0.5.x publishes
# "litestream-<ver>-linux-x86_64.deb". No leading v, and x86_64 rather than
# amd64, which is why the arch case below maps rather than interpolates.
ARG LITESTREAM_VERSION=0.5.16
ARG LITESTREAM_SHA256_AMD64=90cbfeaebda8ea4fed433696ed789e49675b2b4428511173d6d50a84998201fe
ARG LITESTREAM_SHA256_ARM64=aceee9c179a221d072652e810ae19b5a83158df8a29b03e644a719b6b113b80b
RUN set -eu; \
    ARCH=$(dpkg --print-architecture); \
    case "$ARCH" in \
      amd64) EXPECTED="$LITESTREAM_SHA256_AMD64"; ASSET_ARCH=x86_64 ;; \
      arm64) EXPECTED="$LITESTREAM_SHA256_ARM64"; ASSET_ARCH=arm64 ;; \
      *) echo "no pinned litestream checksum for arch $ARCH" >&2; exit 1 ;; \
    esac; \
    wget -q -O /tmp/litestream.deb "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-${LITESTREAM_VERSION}-linux-${ASSET_ARCH}.deb"; \
    echo "${EXPECTED}  /tmp/litestream.deb" | sha256sum -c -; \
    dpkg -i /tmp/litestream.deb; \
    rm /tmp/litestream.deb; \
    litestream version
ENV CHROMEDP_HEADLESS_FLAGS="" \
    CHROMEDP_NO_SANDBOX=1
# Digest-pinned for the same reason as the frontend stage, and it matters more
# here: this is the bun that executes tenant-influenced build scripts
# (bun install + astro build) in production. Index digest, not a platform
# manifest. Pinned 2026-07-29 = bun 1.3.14.
COPY --from=oven/bun:1@sha256:e10577f0db68676a7024391c6e5cb4b879ebd17188ab750cf10024a6d700e5c4 /usr/local/bin/bun /usr/local/bin/bun
# Run as a stable non-root UID:GID 1000:1000. The existing
# atomicsite_atomicsite-data named volume was originally written by an
# alpine system user (100:101); operators upgrading from the alpine
# image must one-shot chown the volume to 1000:1000 before the first
# debian-image deploy:
#   docker run --rm -v atomicsite_atomicsite-data:/data \
#     debian:bookworm-slim chown -R 1000:1000 /data
# 1000:1000 was picked to dodge the GID 101 collision (systemd-journal
# in debian:bookworm-slim) without hardcoding a non-portable system
# UID. New deployments don't need any chown; the entrypoint creates
# /app/data with the right ownership.
RUN groupadd -g 1000 atomicsite && useradd -u 1000 -g atomicsite -m -d /app atomicsite
RUN mkdir -p /app/data && chown -R atomicsite:atomicsite /app
COPY --from=backend /server /app/server
COPY litestream.yml /app/litestream.yml
COPY scripts/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
WORKDIR /app
USER atomicsite
ENV DATA_DIR=/app/data
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
