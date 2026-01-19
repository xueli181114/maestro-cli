ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot

# Build stage
FROM golang:1.25-alpine AS builder

# Git commit passed from build machine (avoids installing git in container)
ARG GIT_COMMIT=unknown

# Install build dependencies
RUN apk add --no-cache make

WORKDIR /build

# Copy source code
COPY . .

# Tidy and verify Go module dependencies
RUN go mod tidy && go mod verify

# Build binary using make to include version, commit, and build date
RUN make build GIT_COMMIT=${GIT_COMMIT}

# Runtime stage
FROM ${BASE_IMAGE}

WORKDIR /app

# Copy binary from builder (make build outputs to bin/)
COPY --from=builder /build/bin/maestro-cli /app/maestro-cli

ENTRYPOINT ["/app/maestro-cli"]

LABEL name="maestro-cli" \
      vendor="Red Hat" \
      version="0.1.0" \
      summary="HyperFleet Maestro CLI" \
      description="Command-line interface for HyperFleet Maestro"
