# Images on an Apple Silicon machine

A pipeline that names an image with no arm64 build fails on this hardware in a
way that does not look like an architecture problem. This is what happens, why,
and the two commands that fix it.

## The symptom

A step's pod sits in `ErrImageNeverPull`, or pulls an image that then refuses to
execute, while `ctr images ls` inside the kind node clearly lists the image. The
image is there. The kubelet still reports it as absent.

## Why

The kubelet's presence check is platform-filtered. An amd64-only image in an
arm64 node's image store is not a usable image for that node, so the kubelet
treats it as missing. With `imagePullPolicy: Never` there is nowhere to go and
the pod stops at `ErrImageNeverPull`; with a policy that permits pulling, the
kubelet fetches the same unusable image again.

`kind load docker-image` does not help. It copies from the local Docker daemon,
which holds the same amd64 image the registry served, so it faithfully copies
the problem into the node.

## The fix

Find a native-arch publication of the same software under a different name, pull
it inside the node so containerd resolves the manifest list for the node's own
platform, then tag it under the name the suite expects. The suite usually cannot
be edited: the image name is written into someone else's test code.

```
oberth preload docker.io/imresamu/postgis:16-3.4 --as postgis/postgis:16-3.4
```

That is the proven case: `postgis/postgis` publishes amd64 only, and
`imresamu/postgis` publishes the same PostGIS versions for arm64. The `--as`
name is what the test asks Testcontainers for.

Without `--as` the command is a plain in-node pull, which is worth having on its
own for a slow image you would rather not wait for during a run.

The command works on every node of the cluster, so a multi-node kind cluster
needs no repetition.

## Interaction with kubedock

`oberth install --testcontainers` deploys kubedock, the Docker API shim that
turns a Testcontainers request into a pod in the pipeline namespace. Run kubedock
with `--pull-policy=never` when you preload this way: it makes kubedock create
test pods with `imagePullPolicy: Never`, so they use exactly the image you put in
the node under exactly the name you gave it, and never re-resolve that name
against the registry that has no arm64 build.

The trade is that a name you did not preload then fails immediately rather than
pulling. On a laptop that is the better failure: it names the missing preload
instead of downloading an image that cannot run.
