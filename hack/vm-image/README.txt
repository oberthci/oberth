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

.oberth/guest-disk.yaml runs this inspection through the normal source plugin
in a run-owned PVC, records the receipt parameter, and collects the scratch PVC
on completion. Select it explicitly; build, promotion and release do not need it.
The receipt proves source integrity, not boot or full walk readiness.

The raw public image is not the fixture disk consumed by .oberth/walk-vm.yaml.
That walk still needs its documented command-line tools, a local upstream and
registry, private disposable identities and /etc/oberth-walk/environment.json.
Do not import a stock disk into oberth-walk-vm and claim those prerequisites.
Cloud-init supplies only the current run's fresh SSH public key.
