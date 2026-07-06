# Build the leilfs-exporter binary
FROM golang:1.21 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/exporter/ cmd/exporter/
COPY internal/exporter/ internal/exporter/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o /workspace/leilfs-exporter ./cmd/exporter

# Final image: leilfs-client carries saunafs-admin, sfsmount, etc.
# We only need saunafs-admin at runtime, but reusing this image avoids
# packaging the upstream client tools twice (and tracking apt sources
# for SaunaFS in another Dockerfile).
FROM ghcr.io/henres/leilfs-container/leilfs-client:5.10.1

# Override the leilfs-client entrypoint, which expects to fork sfsmount.
# Our exporter is a long-running HTTP server and must run as PID 1.
COPY --from=builder /workspace/leilfs-exporter /usr/local/bin/leilfs-exporter

EXPOSE 9418
ENTRYPOINT ["/usr/local/bin/leilfs-exporter"]
