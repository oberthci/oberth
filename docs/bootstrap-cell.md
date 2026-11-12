# Bootstrap acceptance

Branch and release CI now block on the deterministic bootstrap contract cell in
an ordinary rest-test Pod. It verifies signed fetch behavior, the source CLI's
JSON plan and context selection against local TLS/API fixtures, answers/recovery
validation, pinned connect and CA/settings merge, certificate/invite/add-repo
contracts, and scanning of all captured output. It uses no cluster or VM.

The separate fresh-machine entry point installs pinned, verified k3s and Helm,
uses the current certificate-based plugin install/connect path, exercises a held
run in a local disposable Git forge, and performs reboot/unseal verification.
It is intentionally outside CI until a runner can provision a fresh VM and
return authenticated results across reboot. Its VM path has fixture/contract
coverage but has not been qualified on a live fresh machine.

See [the cell instructions](../hack/bootstrap-acceptance/README.md) for exact
commands, provisional host resources, tool pins, output custody and the remaining
runner contract. A green deterministic cell must not be described as successful
end-to-end zero-to-Oberth acceptance.
