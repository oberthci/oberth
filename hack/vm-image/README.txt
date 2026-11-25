The public amd64 Docker guest source is pinned in .oberth/pins/guest-disk.json.
Its versioned URL and both archive hashes receive review together. The SHA-512
comes from the versioned upstream checksum_source; SHA-256 pins the same bytes.
No latest URL, caller-supplied digest or ambient image is accepted.

python3 hack/vm-image/prepare.py --output /absolute/new/disk.img

The script streams the pinned HTTPS archive, verifies both hashes, expands the
raw disk under reviewed size bounds, and atomically publishes a new disk.img.
--archive /absolute/cached.raw.gz permits offline reuse with identical checks.
An existing output refuses. The JSON receipt records archive and raw disk hashes
and the expanded size. No VM starts and nothing is imported or published.

.oberth/walk-vm.yaml reads this pin and script from the admitted commit and
runs them with --pin in a run-named Job and claim in oberth-walk. The VMI boots
only that claim, after the Job succeeded; the exit handler deletes all three.
A changed digest fails the Job, so the VM never starts.

The stock image is not the walk's full fixture. test/walk/walk still needs its
command-line tools, a local upstream and registry, private disposable
identities and /etc/oberth-walk/environment.json; without them that step fails.
Cloud-init supplies only the current run's fresh SSH public key.
