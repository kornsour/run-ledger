# syntax=docker/dockerfile:1

# --- build ---
# No --platform=$BUILDPLATFORM pin here: this build stage is executed once per
# entry in `platforms:` (see .github/workflows/image.yml), natively for each
# target architecture -- via QEMU emulation under buildx for whichever one
# isn't the host's own. That is deliberate, not an oversight. The store now
# has a DuckDB backend (internal/store.DuckDB) that links libduckdb via cgo,
# so `go build` needs a C toolchain that actually matches GOARCH; a single
# host-arch builder cross-compiling GOOS/GOARCH for a foreign target -- the
# CGO_ENABLED=0 approach this Dockerfile used before -- cannot link cgo code
# for an architecture it isn't running on. Emulated native builds are slower
# than a cross-compile, which is the cost of that decision; see
# docs/adr/0006-duckdb-store-backend-and-the-cgo-cost.md for the rest of it.
#
# Debian (glibc), not Alpine (musl): the prebuilt libduckdb this links
# against (github.com/duckdb/duckdb-go-bindings) calls glibc-only symbols
# (backtrace, malloc_trim, the glibc resolver) that musl does not implement
# at all -- statically or dynamically. That is not a static-linking quirk to
# route around; it rules Alpine out as a build image for this binary.
FROM golang:1.26-bookworm AS build
WORKDIR /src

RUN apt-get update && apt-get install -y --no-install-recommends gcc && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-mod go mod download
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/runledger ./cmd/runledger

# distroless has no shell, so there is no `RUN mkdir`/`chown` available in the
# runtime stage below to prepare a writable mount point for the DuckDB
# backend's data directory. Create it here instead, owned by the same
# uid:gid the runtime stage's USER resolves to (distroless nonroot's
# 65532:65532), and COPY it across with ownership preserved. A named volume
# that Compose (or `docker run -v`) mounts empty over /data is seeded from
# whatever is already there in the image, ownership included -- this is what
# makes that first mount writable by the nonroot user instead of root-owned.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# --- runtime ---
# distroless "cc" is the glibc-based variant meant for exactly this: a
# cgo-linked binary that also needs libstdc++ (DuckDB is C++) and libgcc1,
# none of which distroless/static (used before this backend existed) or
# distroless/base (glibc but no libstdc++) provide. Still no shell and no
# package manager. The "nonroot" tag ships an /etc/passwd entry for uid/gid
# 65532 so USER below resolves without a base image that can run useradd.
FROM gcr.io/distroless/cc-debian12:nonroot AS runtime

COPY --from=build /out/runledger /usr/local/bin/runledger
COPY --from=build --chown=65532:65532 /out/data /data

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/runledger"]
