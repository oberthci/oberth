#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(dirname "$0")/.."
export GOTMPDIR=${GOTMPDIR:-$HOME/.cache/go-tmp}
export TMPDIR=$GOTMPDIR
mkdir -p "$GOTMPDIR"
source hack/steps.sh

tests() {
    # The command-line package approaches the default 10-minute limit in landing checks.
    run env OBERTH_REQUIRE_HOLD_NAMESPACES=1 go test -race -count=1 -timeout=20m ./...
    run go -C test/ordering test -race -count=1 -timeout=10m ./...
    run env GOOS=darwin GOARCH=arm64 go vet -p=2 -buildvcs=false ./cmd/oberth/... ./internal/client/... ./internal/installer/... ./agent/...
    run go vet -p=2 -buildvcs=false -tags base_image_qualification ./internal/sshserver ./cmd/oberth-release
    run python3 -I -m unittest discover -s hack -p 'test_history.py'
    run python3 -I -m unittest discover -s hack -p 'test_gate.py'
    run python3 -I -m unittest discover -s hack -p 'test_ui.py'
    run python3 -I -m unittest discover -s hack -p 'test_docs.py'
    run python3 -I -m unittest discover -s hack/guards -p 'test_*.py'
    run python3 -I -m unittest discover -s hack/plans -p 'test_*.py'
    run python3 -I -m unittest discover -s hack/base-image -p 'test_*.py'
}
lint() {
    local pin tool
    pin=$(cat hack/lint-version)
    tool=${GOLANGCI_LINT:-$PWD/hack/lint-tool.sh}
    if [[ $("$tool" version --short) != "${pin#v}" ]]; then
        printf 'FAIL: require linter %s; install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@%s\n' "$pin" "$pin" >&2
        exit 1
    fi
    run "$tool" run --config .golangci.yml ./...
}
guards() {
    python3 -I hack/guards/run.py || exit "$?"
}
case ${1:-check} in
    test) tests ;;
    lint) lint ;;
    lint-selftest) run python3 -I hack/test_lint.py ;;
    guards) guards ;;
    check) run python3 -I hack/plans/parity.py --check; tests; lint; guards; run bash hack/ui-check.sh ;;
    *) printf 'usage: %s test|lint|lint-selftest|guards|check\n' "$0" >&2; exit 2 ;;
esac
