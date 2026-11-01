# Pinned OpenBao chart fixture

`openbao-0.28.6.tgz` is the unchanged public chart downloaded with:

```sh
helm pull openbao --repo https://openbao.github.io/openbao-helm --version 0.28.6
```

SHA256: `175c5cea2d36b68d348eca872044656bd8740c4dbe26b7dc8eb7c7438474a8b3`.
Size: 47,324 bytes. `hack/test-platform-protection.sh` renders this chart to
verify the external release's `server.priorityClassName` and `server.resources`
values without a network connection or cluster access.
