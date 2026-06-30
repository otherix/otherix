# syntax=docker/dockerfile:1.7

# Builds the otherix-gateway binary into an alpine image.
#
# Unlike the agent, a gateway hosts no VMs and execs no qemu, so it is
# genuinely container-deployable: it needs host networking (WireGuard +
# bridge/VXLAN datapath) and CAP_NET_ADMIN, but no KVM device or qemu
# binaries. Run it with --network host and the required capabilities.
ARG GO_VERSION=1.26.4

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/otherix/otherix/internal/version.Version=${VERSION} \
        -X github.com/otherix/otherix/internal/version.Commit=${COMMIT} \
        -X github.com/otherix/otherix/internal/version.Date=${DATE}" \
      -o /out/otherix-gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 otherix
COPY --from=builder /out/otherix-gateway /usr/local/bin/otherix-gateway
USER otherix
ENTRYPOINT ["/usr/local/bin/otherix-gateway"]
