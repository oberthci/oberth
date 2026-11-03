# The caller MUST supply a reviewed complete linux/amd64 QEMU runtime pinned
# by manifest digest. No unverified default, package-manager network, or mutable
# base is used. It must contain /usr/bin/qemu-system-x86_64 and public TLS CAs.
ARG QEMU_RUNTIME
FROM ${QEMU_RUNTIME}
COPY --chmod=0555 oberth-kvm-harness /usr/local/bin/oberth-kvm-harness
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/oberth-kvm-harness"]
