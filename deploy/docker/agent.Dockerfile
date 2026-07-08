# syntax=docker/dockerfile:1.7

# Builds the otherix-agent binary into an alpine image.
#
# NOTE: production agent deployment requires direct host installation
# alongside qemu and KVM device access; this image is suitable only for
# dev / CI / packaging — a containerised agent cannot operate VMs without
# host privileges and qemu binaries that are not in this image.
ARG GO_VERSION=1.26.5

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
      -o /out/otherix-agent ./cmd/agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 otherix
COPY --from=builder /out/otherix-agent /usr/local/bin/otherix-agent
USER otherix
ENTRYPOINT ["/usr/local/bin/otherix-agent"]
