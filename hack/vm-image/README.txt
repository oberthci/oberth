The owner prepares a guest disk with the pinned cluster image and walk tools,
working local upstream and registry fixtures, /etc/oberth-walk/environment.json,
and its disposable fixture credentials. Accounts have locked passwords and SSH password login is disabled.
Cloud-init installs only the current run's fresh SSH public key for the walk user.
prepare.sh verifies the reviewed disk hash and copies it locally. Import the
result as the read-only oberth-walk-vm base PVC in the oberth-walk namespace.
Each admitted VM uses a native ephemeral overlay. No image build, import or
startup is performed by this script. The PVC content pin and tool/version
inventory must be recorded with the prepared image before enabling the cell.
