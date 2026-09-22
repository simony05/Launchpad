FROM golang:1.24-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane ./cmd/control-plane && mkdir -p /out/workspaces

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/control-plane /control-plane
COPY --from=builder --chown=65532:65532 /out/workspaces /workspaces

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/control-plane"]
