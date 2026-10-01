BINARY := oberth
GOENV := GOWORK=off
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
GO_BUILD_FLAGS := -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

.PHONY: build test lint images build-linux-amd64 build-linux-arm64 release-local helm-lint clean

build:
	$(GOENV) CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/$(BINARY) ./cmd/oberth

test:
	$(GOENV) go test -race -count=1 ./...

lint:
	$(GOENV) go vet ./...
	$(GOENV) golangci-lint run ./...

images:
	docker build --file Dockerfile --tag oberth:dev .

build-linux-amd64:
	$(GOENV) CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GO_BUILD_FLAGS) -o dist/oberth-linux-amd64 ./cmd/oberth

build-linux-arm64:
	$(GOENV) CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GO_BUILD_FLAGS) -o dist/oberth-linux-arm64 ./cmd/oberth

release-local: clean build-linux-amd64 build-linux-arm64
	cd dist && sha256sum oberth-linux-amd64 oberth-linux-arm64 > SHA256SUMS

helm-lint:
	./hack/test-chart.sh
	./hack/test-prepare-node.sh

clean:
	rm -rf bin dist
