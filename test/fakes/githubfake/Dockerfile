# Same Go/Git image digest as the reference build workflow.
ARG GO_IMAGE=golang:1.26.10-trixie@sha256:7202554ed358a37f41c44e4910ae29bbc5ef0715f289ac637218adbb0ca132dd
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
ENV GOPROXY=https://proxy.golang.org
RUN go mod download
COPY test/fakes/githubfake/ ./test/fakes/githubfake/
RUN --network=none CGO_ENABLED=0 GOMAXPROCS=2 go build -p=2 -buildvcs=false -trimpath -o /githubfake ./test/fakes/githubfake/cmd/githubfake

FROM ${GO_IMAGE}
RUN --network=none command -v git && test -x "$(git --exec-path)/git-http-backend" && mkdir -p /data && chown 65532:65532 /data
COPY --from=build /githubfake /usr/local/bin/githubfake
USER 65532:65532
EXPOSE 8443 2222
ENTRYPOINT ["/usr/local/bin/githubfake"]
CMD ["-root", "/data", "-ca-out", "/data/ca.pem", "-https", "0.0.0.0:8443", "-control", "127.0.0.1:8080", "-ssh", "0.0.0.0:2222"]
