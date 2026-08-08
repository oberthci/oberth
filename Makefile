BINARY := oberth
RUNNER := oberth-runner
GOENV := GOWORK=off
GO_BUILD_FLAGS := -trimpath -buildvcs=false
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || true)

.PHONY: build test local-flow lint images verify-runner-image provision-runner-trivy provision-runner-crane verify-runner-pin publish-runner-image build-linux-amd64 build-linux-arm64 release-local helm-lint clean

build:
	$(GOENV) CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/$(BINARY) ./cmd/oberth
	$(GOENV) CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/$(RUNNER) ./cmd/oberth-runner

test:
	$(GOENV) go test -race -count=1 ./...

local-flow:
	$(GOENV) go test -race -count=1 -run '^TestLocalFABExampleFlow$$' ./internal/integration

lint:
	$(GOENV) go vet ./...
	$(GOENV) golangci-lint run ./...

images: verify-runner-image
	docker build --file Dockerfile --tag oberth:dev .

verify-runner-image: provision-runner-trivy
	./hack/verify-runner-image.sh

provision-runner-trivy:
	./hack/provision-runner-trivy.sh

provision-runner-crane:
	./hack/provision-runner-crane.sh

verify-runner-pin: provision-runner-crane
	./hack/verify-runner-pin.sh

publish-runner-image: provision-runner-crane
	./hack/publish-runner-image.sh

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
