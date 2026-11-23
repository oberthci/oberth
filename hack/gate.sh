#!/usr/bin/env bash
# Check an immutable branch snapshot with main's tooling in a capped VM.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
source hack/steps.sh
[[ $# == 1 ]] || { printf 'usage: %s <branch>\n' "$0" >&2; exit 2; }
branch=$1
head=unresolved
container=
scratch=
changed=
phase=resolve
phase_started=$SECONDS
started=$SECONDS
docker=(docker --context colima-oberth-next)
next_phase() {
    printf 'gate: phase=%s seconds=%s\n' "$phase" "$((SECONDS-phase_started))"
    phase=$1
    phase_started=$SECONDS
}
cleanup() {
    local status=$? verdict=FAIL final_phase=$phase
    trap - EXIT
    next_phase cleanup
    if [[ -n $container ]]; then
        "${docker[@]}" rm -f "$container" >/dev/null || status=1
    fi
    if [[ -n $scratch ]]; then
        find "$scratch" -depth -delete || status=1
    fi
    printf 'gate: phase=cleanup seconds=%s\ngate: total=%ss\n' "$((SECONDS-phase_started))" "$((SECONDS-started))"
    [[ $status != 0 ]] || verdict=PASS
    printf '\nVERDICT: %s\nBRANCH: %s\nHEAD: %s\nGATE FILES CHANGED: %s\nPHASE: %s\nSTATUS: %s\n' \
        "$verdict" "$branch" "$head" "${changed:-none}" "$final_phase" "$status"
    exit "$status"
}
trap cleanup EXIT
run git check-ref-format --branch "$branch"
head=$(git rev-parse --verify "refs/heads/$branch^{commit}")
if ! git merge-base --is-ancestor main "$head"; then
    printf 'FAIL: branch does not contain current main; merge or rebase onto main and rerun\n' >&2
    exit 1
fi
next_phase tooling
tool_paths=(
    Makefile
    .golangci.yml
    hack/README.md
    hack/check.sh
    hack/ui-check.sh
    hack/webui
    hack/test_ui.py
    hack/dev.sh
    hack/devloop/Dockerfile
    hack/devloop/placeholder/chart/Chart.yaml
    hack/devloop/placeholder/chart/templates/server.yaml
    hack/devloop/placeholder/chart/values.yaml
    hack/devloop/placeholder/main.go
    hack/devloop/placeholder/main_test.go
    hack/gate.sh
    hack/history.py
    hack/lint-version
    hack/parity.sh
    hack/plans/parity.py
    hack/plans/policy.py
    hack/plans/portable.py
    hack/plans/scan.go
    hack/plans/scan_test.go
    hack/plans/test_dependencies.py
    hack/plans/test_portable.py
    hack/plans/test_requirements.py
    hack/plans/test_policy.py
    hack/steps.sh
    hack/test_gate.py
    hack/test_history.py
    hack/test_lint.py
    hack/guards/README.md
    hack/guards/messages.json
    hack/guards/host_paths.json
    hack/guards/main.go
    hack/guards/policy.go
    hack/guards/requirements.go
    hack/guards/main_test.go
    hack/guards/run.py
    hack/guards/test_run.py
)
changed=$(git diff --name-only main "$head" -- "${tool_paths[@]}" docs/plans hack/guards hack/plans hack/devloop | paste -sd ' ' -)
for file in "${tool_paths[@]}"; do
    git cat-file -e "main:$file" 2>/dev/null || { printf 'FAIL: main lacks %s; review and bootstrap main tooling first\n' "$file" >&2; exit 1; }
done
scratch=$(mktemp -d "$GOTMPDIR/gate.XXXXXX")
run git archive --output="$scratch/tooling.tar" main -- "${tool_paths[@]}" docs/plans hack/guards hack/plans
run tar -xf "$scratch/tooling.tar" -C "$scratch"
next_phase history
run python3 "$scratch/hack/history.py" "$head"
next_phase names
[[ -n ${OBERTH_GUARDS_NAMES_FILE:-} ]] || { printf 'FAIL: external names policy is required; set OBERTH_GUARDS_NAMES_FILE\n' >&2; exit 1; }
if git cat-file -e main:test/parity 2>/dev/null && ! git cat-file -e "$head:test/parity" 2>/dev/null; then
    printf 'FAIL: parity harness was removed; restore test/parity\n' >&2; exit 1
fi
run python3 "$scratch/hack/guards/run.py" names "$head"
next_phase prepare
linter=$(command -v "${GOLANGCI_LINT:-golangci-lint}")
image=$(sed -n 's/^ARG GO_IMAGE=//p' "$scratch/hack/devloop/Dockerfile")
cache_prefix=${GATE_CACHE_PREFIX:-oberth-dev}
[[ $cache_prefix =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || { printf 'FAIL: invalid GATE_CACHE_PREFIX\n' >&2; exit 2; }
run "${docker[@]}" image inspect "$image" --format '{{.Id}}'
run git bundle create "$scratch/source.bundle" "$head" refs/heads/main
# Peak container memory plus 25%, rounded up to MiB; see hack/README.md.
# The positional arguments are expanded by the shell inside the VM.
# shellcheck disable=SC2016
container=$("${docker[@]}" create --cpus 2 --memory 7681m --pids-limit 512 \
    --label oberth.gate=true \
    --mount "type=volume,src=$cache_prefix-go-build,dst=/go/cache" \
    --mount "type=volume,src=$cache_prefix-go-mod,dst=/go/pkg/mod" \
    -e GOCACHE=/go/cache -e GOTMPDIR=/go/tmp -e GOMAXPROCS=2 -e OBERTH_GUARDS_NAMES_FILE=/guard-names.txt "$image" \
    bash -ec 'mkdir -p /go/tmp
        git -c init.defaultBranch=main clone -q --bare /source.bundle /repo.git
        git --git-dir=/repo.git worktree add -q --detach /src "$1"
        cd /src
        for dir in hack/guards hack/plans hack/devloop docs/plans hack/webui; do
            if [[ -e $dir || -L $dir ]]; then find "$dir" -depth -delete; fi
        done
        tar -xf /tooling.tar
        git config user.name "$3"
        git config user.email "$4"
        timed() {
            local phase=$1 started=$SECONDS status=0
            shift
            "$@" || status=$?
            printf "gate: phase=%s seconds=%s status=%s\n" "$phase" "$((SECONDS-started))" "$status"
            return "$status"
        }
        timed check make -f Makefile check
        if [[ -n $2 ]]; then timed lint-selftest make -f Makefile lint-selftest; fi
        timed parity bash hack/parity.sh
        if [[ -n $2 ]]; then
            git --git-dir=/repo.git worktree add -q --detach /branch "$1"
            cd /branch
            timed branch-check make -f Makefile check
            timed branch-parity bash hack/parity.sh
        fi
        if [[ -d cmd/oberth ]]; then
            printf "FAIL: product exists; wire install and walk into the gate before merging\n" >&2
            exit 1
        fi
        printf "install: product absent; integration pending\nwalk: product absent; integration pending\n"' \
    gate "$head" "$changed" "$(git config user.name)" "$(git config user.email)")
run "${docker[@]}" cp "$scratch/source.bundle" "$container:/source.bundle"
run "${docker[@]}" cp "$scratch/tooling.tar" "$container:/tooling.tar"
run "${docker[@]}" cp "$linter" "$container:/usr/local/bin/golangci-lint"
run "${docker[@]}" cp -L "$OBERTH_GUARDS_NAMES_FILE" "$container:/guard-names.txt"
next_phase check-and-parity
run timeout --signal=TERM --kill-after=15s 1500 "${docker[@]}" start --attach "$container"
next_phase passed
