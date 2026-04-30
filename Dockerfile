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

