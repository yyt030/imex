# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Install git for fetching dependencies and version stamping
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the binary (static, cross-compilable; version injected into cmd package)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
      -X github.com/gomydump/cmd.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) \
      -X github.com/gomydump/cmd.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
      -X github.com/gomydump/cmd.GitCommit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
    -o gomydump .

# Final stage
FROM alpine:3.20

WORKDIR /app

# Install ca-certificates for SSL/TLS support
RUN apk add --no-cache ca-certificates

# Copy binary from builder
COPY --from=builder /build/gomydump /app/gomydump

# Create backup directory
RUN mkdir -p /backup

# Set working directory
WORKDIR /backup

# Entry point
ENTRYPOINT ["/app/gomydump"]

# Default command shows help
CMD ["--help"]
