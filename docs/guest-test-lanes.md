# Admin-granted guest test leaves

Repository CI can run BPF or filesystem tests as root inside a disposable KVM
guest while its runner pod retains the normal restricted security context.
Declare the leaf in both workflow annotations:

```yaml
oberth.ci/kvm-templates: guest-test
oberth.ci/guest-test-templates: guest-test
```

Admission requires `vm.kvm.enabled` and an active repository KubeVirt admin
grant. This reuses the durable VM capability grant and its transactional
`kubevirt.grant` / `kubevirt.revoke` audit trail; it adds no migration or
repository-controlled grant. The administrator should review the wider VMI
capability that the same grant already authorizes before enabling a repository.
Grant loading uses the durable repository ID on every submission. Missing,
revoked or unreadable grants fail closed. Revocation affects new submissions;
it does not cancel already running workflows.

Only credential-free CI workflows are admitted, covering branch, promotion and
schedule runs. Release workflows, secret paths and WIF leaves are refused.
The normal KVM leaf restrictions also apply: no host cache, token automount,
template defaults, sidecars or init containers. Repository security contexts,
host devices and host namespaces remain forbidden. The host receives only
`devices.kubevirt.io/kvm: 1`, with no added capabilities. The server also sets
`OBERTH_GUEST_TEST=kvm-v1`; consumers must require it to detect old deployments
which do not understand this annotation.

The submission audit records the guest leaf names, repository ID, granted VM
capability, run trigger, source revision and workflow identity. Legacy KVM
acceleration leaves now require the same grant, even without the guest-test
annotation. Removing it cannot bypass VM authorization. This does not convert
their results into trusted evidence.

`packaging/guest-tests` supplies a reference consumer runner. It copies the
static test/init binaries and necessary userspace files into an initramfs,
then boots QEMU with KVM only, two CPUs, 2 GiB, no NIC, no host filesystem share,
no host disk and the QEMU seccomp sandbox. Guest root owns the guest kernel,
cgroups and tracing mounts. BPF tests require a pinned BPF-LSM-capable kernel.
Inputs and kernel hashes enter the log. Guest init fails on any skipped test,
zero passing tests or a nonzero test exit. Host execution fails on timeout,
abnormal QEMU exit or a missing completion record. Cancellation kills and joins
QEMU. Candidate code and guest output are untrusted; this is ordinary CI
coverage, never a substitute for an operator-approved trusted suite receipt.

Consumers vendor the reference sources and compile guest init in their ordinary
build step. The runner image, kernel and toolchain must be digest/version pinned.
No guest privilege is added to the host pod, even if candidate code ignores the
reference runner. Deploy the admission change and provision reviewed repository
grants before expecting consumer runs to pass. No recipe, grant or deployment
is enabled by adding these sources.
