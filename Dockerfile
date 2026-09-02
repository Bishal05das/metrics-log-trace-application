# Multi-stage build.
#
# Stage 1 has the whole Go toolchain — compilers, module cache, source, roughly
# 800MB. Stage 2 keeps only the binary. The final image ships no shell, no
# package manager, and no libc, which is both smaller and a genuinely smaller
# attack surface: an attacker who achieves RCE in your service finds no `sh`,
# no `curl`, and nothing to pivot with.

# ---- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy the module files FIRST and download dependencies as their own layer.
# Docker caches layers by content, so this layer is only rebuilt when go.mod or
# go.sum change — not on every source edit. Without this split, every one-line
# code change re-downloads the entire dependency tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none

# CGO_ENABLED=0 produces a statically linked binary with no libc dependency,
# which is what makes the distroless static image below possible.
#
# -trimpath removes local filesystem paths from the binary, making builds
# reproducible and not leaking your directory layout into production.
#
# -s -w strip the symbol table and DWARF debug info (~30% smaller). Drop them
# if you want usable stack traces from a core dump; panics still show line
# numbers either way.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/api ./cmd/api

# ---- runtime ----------------------------------------------------------------
# distroless/static holds CA certificates, timezone data, /etc/passwd and
# nothing else. `nonroot` runs as uid 65532 rather than root — containers
# should never run as root, and this makes it the default rather than something
# you must remember to configure.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api

# 8086 = API, 9100 = metrics. EXPOSE is documentation only; the ports that
# actually matter are the ones published in docker-compose.yml.
EXPOSE 8086 9100

USER nonroot:nonroot

# No HEALTHCHECK: distroless ships no shell, wget or curl, so there is nothing
# to run one with. That is fine here — Prometheus already scrapes /metrics every
# 5s and `up == 0` is a far better liveness signal than a Docker healthcheck,
# because it also alerts you.

ENTRYPOINT ["/api"]
