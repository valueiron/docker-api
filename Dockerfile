# syntax=docker/dockerfile:1

# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Copy only go.mod first; go mod download fetches all listed modules and writes
# go.sum inside the builder, so no pre-committed go.sum is needed on the host.
COPY go.mod ./
RUN go mod download

# Copy source. go mod tidy then completes go.sum from the cached modules
# (no extra network traffic) before the final static build.
COPY . .
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -ldflags="-w -s" \
      -trimpath \
      -o docker-api \
      .

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
# distroless/static contains no shell, package manager, or other tooling,
# minimising the attack surface.  The nonroot tag sets USER 65532:65532.
FROM gcr.io/distroless/static-debian12:nonroot

# Copy only the compiled binary.
COPY --from=builder /build/docker-api /docker-api

USER nonroot:nonroot

EXPOSE 8080

# The binary doubles as its own health probe via the --healthcheck flag,
# so no external tool (curl, wget) is required in the distroless image.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/docker-api", "--healthcheck"]

ENTRYPOINT ["/docker-api"]
