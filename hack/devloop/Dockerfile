ARG GO_IMAGE=golang@sha256:7202554ed358a37f41c44e4910ae29bbc5ef0715f289ac637218adbb0ca132dd
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY . .
ARG MAIN_PATH=./hack/devloop/placeholder
ENV CGO_ENABLED=0 GOMAXPROCS=2 GOTMPDIR=/go/tmp GOCACHE=/go/cache
RUN --mount=type=cache,id=oberth-go-build,target=/go/cache \
    --mount=type=cache,id=oberth-go-mod,target=/go/pkg/mod \
    mkdir -p /go/tmp && go build -p=2 -buildvcs=false -trimpath -o /server ${MAIN_PATH}
FROM scratch
COPY --from=build /server /server
USER 65532:65532
ENTRYPOINT ["/server"]
