# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build
WORKDIR /src
# Includes go.sum once runtime dependencies are added.
COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/celld-operator ./cmd/celld-operator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/celld-operator /celld-operator
USER 65532:65532
ENTRYPOINT ["/celld-operator"]
