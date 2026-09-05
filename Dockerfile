# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy Go module files
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy application source
COPY main.go ./

# Build application
RUN CGO_ENABLED=0 GOOS=linux go build -o server main.go


# Production stage
FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/server .

# Render provides PORT automatically
EXPOSE 8080

CMD ["./server"]
