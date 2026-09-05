# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy Go module file
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy the complete application source
COPY . .

# Build application
RUN CGO_ENABLED=0 GOOS=linux go build -o server .


# Production stage
FROM alpine:latest

WORKDIR /app

# Copy compiled server
COPY --from=builder /app/server .

# Render provides PORT automatically
EXPOSE 8080

CMD ["./server"]
