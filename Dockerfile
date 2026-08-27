# syntax=docker/dockerfile:1

# --- build ---
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# No third-party modules today (go.sum does not exist), but this still copies
# go.mod first so the module-download layer stays cached once one lands.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 gives a static binary, which is why this single builder can
# cross-compile both target platforms and why the runtime stage below can be
# distroless/scratch with no libc at all.
#
# This works only because the only backend today (internal/store.Memory) is
# pure Go. If the DuckDB backend (issue #1) lands first, it links libduckdb via
# cgo: CGO_ENABLED=0 will then fail to build ./cmd/runledger at all, and
# flipping to CGO_ENABLED=1 needs a C toolchain plus a libduckdb built for
# TARGETARCH, which this single cross-compiling `golang:1.26-alpine` stage does
# not have for a foreign arch. At that point this Dockerfile needs per-arch
# native builders (or a vendored libduckdb layer per platform), not a one-line
# flag flip -- do not just set CGO_ENABLED=1 here and assume arm64 still works.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/runledger ./cmd/runledger

# --- runtime ---
# distroless "static" has no shell, no package manager, and no libc -- exactly
# what a CGO_ENABLED=0 binary needs and nothing more. The "nonroot" variant
# also ships an /etc/passwd entry for uid/gid 65532 so USER below resolves
# without a base image that can run useradd.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/runledger /usr/local/bin/runledger

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/runledger"]
