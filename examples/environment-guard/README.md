# Checking captured process environments

`check.sh ENVIRON_FILE` rejects a finite process-environment capture containing
`PGPASSWORD`. It accepts NUL-separated input and drains the complete capture so
`set -o pipefail` cannot turn a detected secret into a false success.

Use this pattern for workload-owned integration assertions in an Oberth pipeline.
The script only checks its input; workload setup and enforcement verification
belong in the application repository. `hack/test-environment-guard.sh` proves both
outcomes, including a large capture with a matching first record.
