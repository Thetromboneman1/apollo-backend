# syntax=docker/dockerfile:1.6

FROM golang:1.26.7-alpine3.24 AS builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/apollo \
    ./cmd/apollo

FROM alpine:3.24.1
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S -g 10001 apollo && \
    adduser -S -D -H -u 10001 -G apollo apollo

COPY --from=builder --chown=10001:10001 /out/apollo /usr/local/bin/apollo

USER 10001:10001
EXPOSE 4000

ENTRYPOINT ["/usr/local/bin/apollo"]
