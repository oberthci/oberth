# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS build
ENV GOTOOLCHAIN=local GOWORK=off
ARG BUILD_CACHE_NAMESPACE=default

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gomod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=oberth-server-${BUILD_CACHE_NAMESPACE}-gobuild,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/oberth ./cmd/oberth

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40
RUN apk add --no-cache \
    ca-certificates=20260611-r0 \
    git=2.52.0-r0 \
    openssh-client-default=10.2_p1-r0 \
    tzdata=2026c-r0 \
    && rm -f /var/log/apk.log
COPY --from=build /out/oberth /usr/local/bin/oberth
ENV HOME=/tmp
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/oberth"]
CMD ["serve"]
