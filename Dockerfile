# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# The build stage runs on the build host's native platform and cross-compiles
# with GOOS/GOARCH; only the runtime stage below is per-target-platform.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.23@sha256:a07f231c19b03e6106cf442f02a5bcf262147e744d15b7b5e4b6d81d420c70a0 AS build
ENV GOTOOLCHAIN=local GOWORK=off
ARG BUILD_CACHE_NAMESPACE=default

# The runtime copies only this trust-store data, never apk metadata or tools.
RUN apk add --no-cache ca-certificates=20261020-r0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
# VERSION feeds `oberth version` and the dashboard/status version display;
# release builds pass the exact tag, dev builds keep "dev".
ARG VERSION=dev
RUN --mount=type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gobuild,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /out/oberth ./cmd/oberth && \
    ln -s oberth /out/oberth-git-ssh

# Git uses the target platform's musl compiler; no Git transport libraries or
# shell tools enter the runtime. A source change must update both the tar hash
# and the reviewed local-transport patch before rebuilding the substrate.
FROM --platform=$TARGETPLATFORM golang:1.26.10-alpine3.23@sha256:a07f231c19b03e6106cf442f02a5bcf262147e744d15b7b5e4b6d81d420c70a0 AS git-build
COPY internal/staticgit/build.env /pins/static-git.env
COPY hack/install-static-git-packages.sh /tmp/install-static-git-packages.sh
RUN /bin/sh /tmp/install-static-git-packages.sh /pins/static-git.env
COPY hack/static-git-no-shell.patch /tmp/static-git-no-shell.patch
COPY hack/build-static-git.sh /tmp/build-static-git.sh
RUN /bin/sh /tmp/build-static-git.sh /pins/static-git.env /tmp/static-git-no-shell.patch /out

# Rebuild both platform substrates and hand off their exact index before
# changing the release pin. The source Git CLI is the only serving backend.
FROM scratch AS runtime
LABEL org.oberth.static-git.version="2.56.0" \
      org.oberth.static-git.source-sha256="26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3" \
      org.oberth.static-git.musl="1.2.5-r24" \
      org.oberth.static-git.zlib="1.3.2-r1"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chmod=0755 /out/ /usr/local/bin/
COPY --from=git-build --chmod=0755 /out/git /usr/bin/git
ENV HOME=/data TMPDIR=/tmp PATH=/usr/local/bin:/usr/bin
WORKDIR /data
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/oberth"]
CMD ["serve"]
