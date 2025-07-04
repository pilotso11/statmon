# Stage 1: Build the Go application
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code
COPY . .

# Build the application
# CGO_ENABLED=0 is often used for static binaries, especially with scratch.
# gopsutil might require CGO for some features or specific OS.
# For Alpine, it's generally better to build with CGO_ENABLED=1 (default)
# or ensure all necessary static libraries are linked if CGO_ENABLED=0.
# Let's try with CGO_ENABLED=0 first for a smaller binary, if it fails we can adjust.
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /go-monitor main.go

# Stage 2: Create the final lightweight image
FROM alpine:latest

# gopsutil might need ca-certificates and potentially other libs for full functionality
# depending on what features are used and how it's built.
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the built binary from the builder stage
COPY --from=builder /go-monitor /app/go-monitor

# The application uses embedded config and static assets, so no need to copy them separately
# unless an external config.yaml is desired at /app/config/config.yaml

# Create a non-root user to run the application
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

# Expose the port the application listens on (default 8080)
EXPOSE 8080

# Set default environment variables for configuration (can be overridden)
# ENV MONITOR_SERVER_PORT=8080
# ENV MONITOR_SERVER_HOST="0.0.0.0"
# (Viper will pick these up if set as MONITOR_SERVER_PORT or SERVER_PORT etc based on AutomaticEnv and replacer)

# Define the entrypoint for the container
ENTRYPOINT ["/app/go-monitor"]

# Default command (can be empty if ENTRYPOINT is sufficient)
# CMD [""]
