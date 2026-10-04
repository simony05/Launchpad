FROM golang:1.24-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane ./cmd/control-plane

FROM docker:27-cli AS docker-cli

FROM alpine:3.21

RUN addgroup -S launchpad && adduser -S -G launchpad launchpad && mkdir /workspaces && chown launchpad:launchpad /workspaces

COPY --from=builder /out/control-plane /control-plane
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker

EXPOSE 8080

USER launchpad

ENTRYPOINT ["/control-plane"]
