FROM golang:1.24-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane ./cmd/control-plane \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM docker:27-cli AS docker-cli

FROM alpine:3.21 AS runtime

RUN apk add --no-cache ca-certificates && \
    addgroup -S launchpad && \
    adduser -S -G launchpad launchpad && \
    mkdir -p /workspaces /usr/local/libexec/docker/cli-plugins && \
    chown launchpad:launchpad /workspaces

FROM runtime AS worker

COPY --from=builder /out/worker /worker
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx

EXPOSE 8090

USER launchpad

ENTRYPOINT ["/worker"]

FROM runtime AS control-plane

COPY --from=builder /out/control-plane /control-plane

EXPOSE 8080

USER launchpad

ENTRYPOINT ["/control-plane"]
