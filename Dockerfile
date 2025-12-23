# Build Stage
FROM golang:1.22-alpine AS builder

# Install build dependencies (needed for CGO/SQLite)
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
# CGO_ENABLED=1 is required for go-sqlite3
RUN CGO_ENABLED=1 GOOS=linux go build -o server cmd/server/main.go

# Runtime Stage
FROM alpine:latest

WORKDIR /app

# SQLite requires libc (musl is in alpine)
# We might need ca-certificates if making outbound HTTPS requests later
RUN apk add --no-cache ca-certificates sqlite

COPY --from=builder /app/server .

# Expose ports
# 4443: Control Plane
# 8080: Ingress (mapped to 80 on host)
# 443: Ingress (HTTPS)
EXPOSE 4443 8080 443

# Volume for database persistence
VOLUME ["/app/data"]

# Entrypoint
CMD ["./server"]
