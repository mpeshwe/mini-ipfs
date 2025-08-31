# Build stage
FROM golang:1.24-alpine AS builder

# Install git and ca-certificates (needed for go mod download)
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Copy go mod files first for better Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bin/mini-ipfs-node ./cmd/node

# Final stage
FROM alpine:latest

# Install ca-certificates for HTTPS requests and wget for health checks
RUN apk --no-cache add ca-certificates wget

# Create non-root user for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/bin/mini-ipfs-node .

# Create data directory and set ownership
RUN mkdir -p /data && chown appuser:appgroup /data

# Change to non-root user
USER appuser

# Expose ports
EXPOSE 8080 7000

# Set default environment variables
ENV MINI_IPFS_NODE_HTTP_ADDR=:8080
ENV MINI_IPFS_NODE_DHT_ADDR=:7000
ENV MINI_IPFS_NODE_DATA_DIR=/data

# Health check
# HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
#   CMD wget --quiet --tries=1 --spider http://localhost:8080/health || exit 1

# Run the application
CMD ["./mini-ipfs-node"]