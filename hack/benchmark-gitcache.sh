#!/bin/sh
# Read the source mirror only; all fetch/checkout/maintenance writes go to a
# newly-created results directory. Use --synthetic instead of a source path
# to generate the #1205 60k-commit/200k-object fixture (~1.5 GiB pack).
set -eu
use_docker=false
compact=false
synthetic=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --docker) use_docker=true; shift ;;
    --compact) compact=true; shift ;;
    --synthetic) synthetic=true; shift ;;
    *) break ;;
  esac
done
if $synthetic; then
  [ "$#" -eq 1 ] || { printf 'usage: %s [--docker] [--compact] --synthetic NEW_RESULTS_DIR\n' "$0" >&2; exit 2; }
  source_path=
  results=$1
else
  [ "$#" -eq 2 ] || { printf 'usage: %s [--docker] [--compact] BARE_MIRROR NEW_RESULTS_DIR\n' "$0" >&2; exit 2; }
  source_path=$(cd "$1" && pwd -P)
  results=$2
fi
[ ! -e "$results" ] || { printf 'results directory must not exist: %s\n' "$results" >&2; exit 2; }
mkdir -p "$results"
results=$(cd "$results" && pwd -P)
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
if $synthetic; then
  python3 "$repo_root/hack/gitcache-bench/generate.py" "$results/source.git" > "$results/generate.log" 2>&1
  source_path=$results/source.git
fi
git -C "$source_path" rev-parse HEAD > "$results/source-head.txt"
git -C "$source_path" count-objects -v > "$results/source-objects.txt"
export GOTOOLCHAIN=go1.26.10
export GOMAXPROCS=${BENCH_CPUS:-4}
if $use_docker; then
  architecture=$(docker info --format '{{.Architecture}}')
  case "$architecture" in arm64|aarch64) architecture=arm64 ;; amd64|x86_64) architecture=amd64 ;; *) printf 'unsupported architecture: %s\n' "$architecture" >&2; exit 2 ;; esac
  (cd "$repo_root" && GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -o "$results/bench" ./hack/gitcache-bench)
else
  (cd "$repo_root" && go build -trimpath -o "$results/bench" ./hack/gitcache-bench)
fi
run_measurement() {
  backend=$1
  label=$2
  operation=$3
  if $use_docker; then
    docker run --rm --network=none --memory="${BENCH_MEMORY:-8g}" --cpus="${BENCH_CPUS:-4}" \
      --mount "type=bind,src=$results/bench,dst=/usr/local/bin/bench,readonly" \
      --mount "type=bind,src=$source_path,dst=/source.git,readonly" \
      --mount "type=bind,src=$results/$backend,dst=/cache" \
      -e "GOMAXPROCS=$GOMAXPROCS" \
      golang:1.26.10-trixie@sha256:7202554ed358a37f41c44e4910ae29bbc5ef0715f289ac637218adbb0ca132dd \
      bench -cgroup-isolated -backend "$backend" -remote /source.git -root /cache -operation "$operation" \
      > "$results/$backend-$label.json" 2> "$results/$backend-$label.log"
  else
    "$results/bench" -backend "$backend" -remote "$source_path" -root "$results/$backend" -operation "$operation" \
      > "$results/$backend-$label.json" 2> "$results/$backend-$label.log"
  fi
}
for backend in cli go-git; do
  mkdir "$results/$backend"
  run_measurement "$backend" cold ensure
  run_measurement "$backend" warm ensure
  run_measurement "$backend" checkout checkout
  run_measurement "$backend" automatic-gc gc
  if $compact; then
    run_measurement "$backend" compact compact
  fi
done
printf 'measurements saved in %s\n' "$results"
