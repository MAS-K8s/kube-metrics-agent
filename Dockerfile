# syntax=docker/dockerfile:1.7

############################
# Build stage
############################
FROM golang:1.25-alpine AS build

RUN apk add --no-cache ca-certificates git

WORKDIR /src

# cache deps
COPY go.mod go.sum ./
RUN go mod download

# copy source
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

# build static binary
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/rl-controller ./


############################
# Runtime stage (lightweight)
############################
FROM alpine:3.20

# add certs + nonroot user
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 -h /home/app app

WORKDIR /

# copy binary
COPY --from=build /out/rl-controller /rl-controller

# run as non-root
USER 10001:10001

# Old controller doesn't expose HTTP, so do NOT expose 8080
# EXPOSE 8080

# optional: graceful stop (K8s sends SIGTERM)
STOPSIGNAL SIGTERM

ENTRYPOINT ["/rl-controller"]





# # syntax=docker/dockerfile:1.7

# FROM golang:1.23-alpine AS build
# RUN apk add --no-cache ca-certificates git
# WORKDIR /src
# COPY go.mod go.sum ./
# RUN go mod download
# COPY . .
# ARG TARGETOS=linux
# ARG TARGETARCH=amd64
# RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
#     go build -trimpath -ldflags="-s -w" -o /out/rl-controller ./

# FROM alpine:3.20
# RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
# WORKDIR /
# COPY --from=build /out/rl-controller /rl-controller
# USER app
# EXPOSE 8080
# ENTRYPOINT ["/rl-controller"]




# # Multi-stage build for Go Controller
# FROM golang:1.21-alpine AS builder

# # Install build dependencies
# RUN apk add --no-cache git ca-certificates

# # Set working directory
# WORKDIR /app

# # Copy go mod files
# COPY go.mod go.sum ./

# # Download dependencies
# RUN go mod download

# # Copy source code
# COPY . .

# # Build the application
# RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o controller main.go

# # Final stage - minimal image
# FROM alpine:latest

# # Install ca-certificates for HTTPS calls
# RUN apk --no-cache add ca-certificates

# WORKDIR /root/

# # Copy binary from builder
# COPY --from=builder /app/controller .

# # Expose port (if needed for health checks)
# EXPOSE 8080

# # Run the binary
# CMD ["./controller"]