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

Cloud-init embeds bootstrap.py and .oberth/pins/walk-tools.json from the same
admitted source. It installs digest-verified tools, pulls digest-pinned local
fixtures, creates disposable identities and /etc/oberth-walk/environment.json.
The Docker context names only the guest's local socket. Credentials stay in
the guest; cloud-init contains only public inputs and the fresh SSH public key.
The disk is sparsely enlarged to 32 GiB; cloud-init grows the root filesystem.
The claim reserves 40 GiB for the original and ephemeral disk writes.
The walk waits for cloud-init and checks every required tool and environment
file before installing the candidate. A failed bootstrap blocks the walk.
Provisioning needs guest DNS/HTTPS egress, declared in the walk setup policy.
The reviewed nameservers in walk-tools.json replace the minimal guest's absent
stub resolver. Git's native SSH commands use a digest-verified distribution
package and the pinned guest's libc, PCRE and zlib; no package resolver runs.
No VM is needed for the offline provisioning and missing-tool controls:
python3 -I -m unittest discover -s hack/vm-image -p 'test_*.py'

Check the registry command with the real pinned parser, without a daemon:
python3 -I -B hack/vm-image/check_k3d.py --k3d /absolute/pinned/k3d
This verifies the executable's checksum before executing its help path.

Check remote availability separately from the offline suite (requires crane):
python3 -I -B hack/vm-image/check_image_pins.py
This anonymously fetches every exact pinned manifest, verifies its SHA-256,
and exits nonzero on an unavailable digest, checksum mismatch or network error.
It reads no operator registry configuration and uses no container daemon.
