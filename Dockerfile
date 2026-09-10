# Build stage
FROM golang:1.27@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS build
WORKDIR /src

# Download dependencies first for better layer caching
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy source and build. VERSION/COMMIT feed the playwright_build_info metric.
ARG VERSION=dev
ARG COMMIT=none
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
    -ldflags="-s -w -extldflags \"-static\" -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/playwright-proxy ./cmd/playwright-proxy

# Runtime stage - use distroless for minimal attack surface
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Copy binary with appropriate permissions
COPY --from=build --chown=nonroot:nonroot /out/playwright-proxy /usr/local/bin/playwright-proxy

# Use non-root user (UID/GID 65532)
USER nonroot:nonroot

# Expose ports
EXPOSE 9000 9090

# Health check endpoint on management port
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/playwright-proxy", "healthz"]

ENTRYPOINT ["/usr/local/bin/playwright-proxy"]
