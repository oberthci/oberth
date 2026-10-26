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
    go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /out/oberth ./cmd/oberth

# One-release compatibility target for --git-backend=cli. It is never the
# release substrate or the default/final image; build --target cli-runtime
# explicitly when exercising the fallback.
FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS cli-runtime
# These exact pins apply only to the optional CLI compatibility image.
# The normal release has no OS packages; never pin this target as its substrate.
RUN apk add --no-cache \
    ca-certificates=20261020-r0 \
    git=2.52.0-r0 \
    # CVE-2026-14456 (HIGH): the alpine:3.23 base at the digest above still
    # ships libcrypto3/libssl3 3.5.7-r0. The fixed 3.5.9-r0 is available for
    # x86_64 and aarch64 (and supersedes the prior 3.5.8-r0 pin). Exact-pin the
    # current repository revision so a superseding version fails the build
    # loudly and is deliberately reviewed — never silently drift.
    libcrypto3=3.5.9-r0 \
    libssl3=3.5.9-r0 \
    # CVE-2026-92392 (HIGH): HTTP/2 upload use-after-free affects the prior
    # libcurl 8.22.0-r0 pin. The fixed 8.23.0-r0 is available for x86_64 and
    # aarch64 and retains the earlier libcurl security fixes. Exact-pinned
    # like libcrypto3 above: when the repository supersedes this revision,
    # fail the build loudly and re-pin deliberately — never drift silently.
    libcurl=8.23.0-r0 \
    # CVE-2026-103111 (HIGH): pcre2 arrives as a runtime dependency; the
    # digest-pinned Alpine base still contains 10.48-r0 while 10.49-r0 is
    # fixed and available for x86_64 and aarch64. Pin the fixed APK revision
    # so substrate builds fail loudly if the repository supersedes it.
    pcre2=10.49-r0 \
    # CVE-2026-93990, CVE-2026-77214 (HIGH): libexpat arrives as git's
    # transitive dependency; the alpine:3.23 base digest above still resolves
    # 2.8.4-r0 while the fixed 2.9.0-r0 exists only in the apk repository
    # (verified present for x86_64 and aarch64). Exact-pinned like libcrypto3
    # above: when the repo supersedes 2.9.0-r0 this line fails the build
    # loudly — re-pin deliberately, never drift silently.
    libexpat=2.9.0-r0 \
    # CVE-2026-111497 (HIGH): brotli-libs arrives as libcurl's transitive
    # dependency; the alpine:3.23 base digest above still resolves 1.2.0-r0
    # while the fixed 1.2.1-r0 exists only in the apk repository (verified
    # present for x86_64 and aarch64). Exact-pinned like libcrypto3 above.
    brotli-libs=1.2.1-r0 \
    openssh-client-default=10.2_p1-r0 \
    tzdata=2026e-r0 \
    && rm -f /var/log/apk.log
COPY --from=build /out/oberth /usr/local/bin/oberth
ENV HOME=/tmp
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/oberth"]
CMD ["serve"]

# SUBSTRATE REBUILD REQUIRED: release images repack ServerSubstrate in
# internal/releaseimage/image.go. After changing this final stage or CA data,
# build both architectures, check ELF architecture, zero OS packages, Repack
# and Trivy; publish the exact index with preserved digests and commit its new
# ServerSubstrate digest. Never pin the compatibility target above.
FROM scratch AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/oberth /usr/local/bin/oberth
ENV HOME=/data TMPDIR=/tmp PATH=/usr/local/bin
WORKDIR /data
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/oberth"]
CMD ["serve"]
