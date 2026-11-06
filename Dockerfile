# syntax=docker/dockerfile:1
# The whole app in one image: the static composewright binary and the static
# docker CLI and compose plugin it validates stacks with, on distroless as
# uid 65532, with /data as the only volume. The Go stage runs on the build
# machine and cross-compiles, so a multi-platform build needs no emulation.
# Every base image is pinned by digest.

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.10-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=none DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY web web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o /out/composewright ./cmd/composewright \
 && mkdir -m 0700 /out/data

FROM docker.io/library/docker:29.9.2-cli@sha256:5e6baf67325a79f64ed3bed6863954d5e05d0f3d4e57cbdb14809346814bbfdb AS docker

# Upstream Compose 5.7.0 still bundles vulnerable x/net/html. Rebuild its
# checksum-pinned source with patched x/net and compression dependencies.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.10-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS compose
WORKDIR /fetch-compose
RUN go mod download -json github.com/docker/compose/v5@v5.7.0 > source.json \
 && grep -F '"Sum": "h1:nRBxm4HkCFo8Bgb49o6xdhnJ0nl1eGb1H0FAb/ExldE="' source.json \
 && cp -a /go/pkg/mod/github.com/docker/compose/v5@v5.7.0 /compose \
 && chmod u+w /compose/go.mod /compose/go.sum
WORKDIR /compose
RUN go mod edit -require=golang.org/x/net@v0.62.0 -require=github.com/klauspost/compress@v1.20.2 \
 && go mod download golang.org/x/net github.com/klauspost/compress
ARG TARGETOS TARGETARCH
RUN mkdir -p /out \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=mod -trimpath -buildvcs=false \
      -tags e2e -ldflags "-s -w -X github.com/docker/compose/v5/internal.Version=v5.7.0-composewright.1" \
      -o /out/docker-compose ./cmd

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG VERSION=dev
LABEL org.opencontainers.image.title="composewright" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.licenses="MIT" \
      io.composewright.compose.version="5.7.0-composewright.1" \
      org.opencontainers.image.source="https://github.com/aiulian25/composewright"
COPY --from=build /out/composewright /usr/bin/composewright
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=compose /out/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
# composewright refuses a data directory that is not 0700 and its own, so the
# volume starts out that way.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data
ENV COMPOSEWRIGHT_DATA_DIR=/data COMPOSEWRIGHT_CONFIG=/data/config.yaml DOCKER_CONFIG=/tmp/docker COMPOSEWRIGHT_RUN_AS=65532:65532
# It starts as root only to make a folder Docker created for /data its own and
# to join the Docker socket's group, then runs as COMPOSEWRIGHT_RUN_AS with no
# capabilities.
USER 0:0
VOLUME ["/data"]
EXPOSE 8484
ENTRYPOINT ["/usr/bin/composewright"]
CMD ["serve", "--init", "--listen", "0.0.0.0:8484"]
