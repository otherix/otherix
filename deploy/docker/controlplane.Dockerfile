# syntax=docker/dockerfile:1.7

# Distroless image for otherix-api. The control plane is a single
# binary now — placement runs in-process under store.LockKeyPlacement,
# reconcilers run as river periodic workers inside the same process.
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
      -o /out/otherix-api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/otherix-api /usr/local/bin/otherix
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/otherix"]
