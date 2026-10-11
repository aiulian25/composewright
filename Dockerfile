# syntax=docker/dockerfile:1
# The whole app in one image: the static composewright binary and the static
# docker CLI and compose plugin it validates stacks with, on distroless as
# uid 65532, with /data as the only volume. The Go stage runs on the build
# machine and cross-compiles, so a multi-platform build needs no emulation.
# Every base image is pinned by digest.

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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

FROM docker.io/library/docker:29.9.0-cli@sha256:1a4c7cb63513f349bdad01fcc6e0f3f2f67d37b9da86f14dc0d4a0942eecda00 AS docker

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG VERSION=dev
LABEL org.opencontainers.image.title="composewright" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/aiulian25/composewright"
COPY --from=build /out/composewright /usr/bin/composewright
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
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
