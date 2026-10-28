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
COPY third_party/go-git ./third_party/go-git
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
RUN apk add --no-cache build-base zlib-dev zlib-static patch linux-headers ca-certificates=20261020-r0
WORKDIR /git
RUN wget -qO /tmp/git.tar.xz https://www.kernel.org/pub/software/scm/git/git-2.56.0.tar.xz && \
    echo "26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3  /tmp/git.tar.xz" | sha256sum -c - && \
    tar -xJf /tmp/git.tar.xz --strip-components=1 && rm /tmp/git.tar.xz
COPY hack/static-git-no-shell.patch /tmp/static-git-no-shell.patch
RUN patch -p1 --fuzz=0 < /tmp/static-git-no-shell.patch && \
    make -j2 git prefix=/usr CFLAGS="-O2 -ffile-prefix-map=/git=." LDFLAGS=-static \
      NO_CURL=YesPlease NO_OPENSSL=YesPlease NO_EXPAT=YesPlease \
      NO_PERL=YesPlease NO_PYTHON=YesPlease NO_TCLTK=YesPlease NO_GETTEXT=YesPlease \
      NO_ICONV=YesPlease NO_LIBPCRE2=YesPlease NO_REGEX=YesPlease NO_RUST=YesPlease && \
    mkdir /out && cp git /out/git && strip /out/git && \
    ! readelf -l /out/git | grep -q INTERP && ! readelf -d /out/git | grep -q NEEDED

# Rebuild both platform substrates and hand off their exact index before
# changing the release pin. The source Git CLI is the only serving backend.
FROM scratch AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chmod=0755 /out/ /usr/local/bin/
COPY --from=git-build --chmod=0755 /out/git /usr/bin/git
ENV HOME=/data TMPDIR=/tmp PATH=/usr/local/bin:/usr/bin
WORKDIR /data
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/oberth"]
CMD ["serve"]
