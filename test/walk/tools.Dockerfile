FROM golang:1.26.10-alpine3.23@sha256:a07f231c19b03e6106cf442f02a5bcf262147e744d15b7b5e4b6d81d420c70a0
RUN apk add --no-cache git python3 ca-certificates
COPY cosign /usr/local/bin/cosign
COPY oberth /usr/local/bin/oberth
ENV HOME=/tmp GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod GOTOOLCHAIN=local
