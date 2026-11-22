# Oberth

Oberth is a thin CI/CD front door over Kubernetes and Argo Workflows. You push a commit; Oberth
checks your repository's own workflow YAML and runs it. Oberth never schedules, queues, orders or
rate-limits runs; Argo runs them.

CI means building and testing each change; CD means delivering the tested commit. Oberth runs on
your cluster, for you and your agents.

## First green build

Start with a Kubernetes context you control, a trusted Oberth CLI, Git, and a checkout with
`.oberth/build.yaml` committed. A forge is the server that stores your upstream Git repository. The
checked-in fetcher has no published signing key and cannot obtain the CLI at this revision. Before
continuing, obtain a signed release and independently trusted public pin from the maintainer. See
[installation](docs/install.md) for verified acquisition once published. Prepare the build with
[pipelines](docs/pipelines.md); `oberth init` supplies a starter. These commands assume both are
ready. Replace angle-bracket placeholders before running.

Before installing into a cluster that already has Oberth, use the
[second-installation procedure](docs/install.md#a-second-installation-in-the-same-cluster). The
default command below is for a first installation.

1. Install. Supply the reachable HTTPS address, read the review and confirm when ready:

   ```sh
   oberth install --context <context> --url <https-url>
   ```

   With `--context` supplied, install uses defaults for omitted connection settings;
   it does not ask for an address. Supply `--ssh-port <port>` too if the default is unreachable.
   Add the printed public deploy key to your forge with write access.
   Copy the saved `backup.agekey` off this machine; keep its contents private.
   Install saves your administrator profile: local connection details and credentials,
   including the Oberth server's SSH host keys, which `repo add` uses by itself.

2. Register the repository and bind this checkout, with the forge's own SSH URL:

   ```sh
   oberth repo add <forge-url>
   ```

   This first attempt stops and prints the forge's offered SSH host fingerprints.
   Compare one with the host keys in the forge's official SSH documentation or the list
   supplied by its administrator. After verifying it, register and bind the checkout:

   ```sh
   oberth repo add <forge-url> --expected-host-fingerprint <fingerprint>
   ```

   Use `--repin` only after a changed forge key has been independently verified.
   Use the full repository name it prints; its first path component is the upstream name.
   The forge's key is pinned by the server; it is never read from your own SSH files.

3. Create an [uplink](docs/agents.md) for the approved upstream. Join with its file:

   ```sh
   oberth invite <upstream>
   oberth join <invitation-file>
   ```

   Join switches this checkout, bound in step 2, to the uplink and keeps your administrator
   profile selected. It binds no other clone; there, run `oberth repo add <forge-url>`.

4. If migrating issues, finish the [issue import](docs/issue-import.md) now, before any
   CI-enabled push can create native issue numbers. Then push a branch, follow the returned
   run ID, and read its final state. Treat this push as the start of CI on the new installation.
   Keep the previous installation available until import and the new CI/release path are verified;
   switching off its CI is a separate migration decision:

   ```sh
   git push oberth HEAD:refs/heads/change -o oberth.run=1
   oberth log <run>
   oberth run wait <run>
   ```

Eight commands; an agent's separate install review adds one. Port and flag counts are in
[installation](docs/install.md#command-counts). `run wait` exits 0 even if the run failed. Open the
printed watch link and [sign in](docs/operations.md#browser-access) to see steps and logs.

## First release

Follow the [project release steps](docs/releasing.md#release-your-project): commit the publisher,
grant secrets as administrator, promote, synchronize the checkout and wait for passing CI, then
release. With a matching checkout this takes three Oberth commands: `secret put`, `promote` and
`release`; omit `secret put` when no secret is needed. Synchronization, grants and retries add
commands.

## What Oberth does not do

See [security](docs/security.md) and [recovery](docs/install.md#back-up) for the limits.
