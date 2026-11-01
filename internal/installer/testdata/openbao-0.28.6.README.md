# Pinned OpenBao chart fixture

`openbao-0.28.6.tgz` is the unchanged public chart downloaded with:

```sh
helm pull openbao --repo https://openbao.github.io/openbao-helm --version 0.28.6
```

SHA256: `175c5cea2d36b68d348eca872044656bd8740c4dbe26b7dc8eb7c7438474a8b3`.
Size: 47,324 bytes. `hack/test-platform-protection.sh` renders this chart to
verify the external release's `server.priorityClassName` and `server.resources`
values without a network connection or cluster access.

Upstream source: <https://github.com/openbao/openbao-helm> (chart version
0.28.6). The upstream chart is **MPL-2.0**, separate from this repository's
Apache-2.0 license. Its archive omits the license file; the upstream
[license](https://github.com/openbao/openbao-helm/blob/main/LICENSE) is included
alongside it as [openbao-0.28.6.LICENSE](openbao-0.28.6.LICENSE), preserving the
HashiCorp copyright notice. The test checks the archive SHA256 before rendering.
