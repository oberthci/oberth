#!/bin/bash
# Prepare pinned QEMU and a BPF-LSM kernel. Never installs host packages.
set -euo pipefail
KT=${1:?guest output directory required}
mkdir -p "$KT/qemu" "$KT/kernel"
APT_DIR="$KT/apt"
mkdir -p "$APT_DIR"/state/lists/partial "$APT_DIR"/cache "$APT_DIR"/debs
apt-get -o Dir::State="$APT_DIR/state" \
        -o Dir::Cache="$APT_DIR/cache" \
        -o APT::Sandbox::User=root update -qq
(
  cd "$APT_DIR/debs"
  apt-get -o Dir::State="$APT_DIR/state" \
          -o Dir::Cache="$APT_DIR/cache" \
          -o APT::Sandbox::User=root \
      download \
      qemu-system-x86=1:10.0.13+ds-0+deb13u1 \
      qemu-system-common=1:10.0.13+ds-0+deb13u1 \
      qemu-system-data=1:10.0.13+ds-0+deb13u1 \
      seabios=1.16.3-2 \
      ipxe-qemu=1.21.1+git20250501.dad20602+dfsg-1 \
      libglib2.0-0t64=2.84.4-3~deb13u5 libelf1t64=0.192-4 \
      libaio1t64=0.3.113-8+b1 libcapstone5=5.0.7-1~deb13u1 \
      libfdt1=1.7.2-2+b1 libfuse3-4=3.17.2-3 \
      libibverbs1=56.1-1 libjpeg62-turbo=1:2.1.5-4 \
      libnuma1=2.0.19-1 libpixman-1-0=0.44.0-3 \
      libpmem1=1.13.1-1.1+b1 librdmacm1t64=56.1-1 \
      libslirp0=4.8.0-1+deb13u1 liburing2=2.9-1 \
      libvdeplug2t64=4.0.1-5.1+b1 \
      libdaxctl1=81-1 libndctl6=81-1 libkmod2=34.2-2 \
      libnl-3-200=3.7.0-2 libnl-route-3-200=3.7.0-2 \
      libpng16-16t64=1.6.48-1+deb13u5 libbpf1=1:1.5.0-3 \
      libasound2t64=1.2.14-1+deb13u1 libusb-1.0-0=2:1.0.28-1 \
      libusbredirparser1t64=0.15.0-1 \
      libcacard0=1:2.8.0-3+b2 libbrlapi0.8=6.7-3.1+deb13u3 \
      libnspr4=2:4.36-1 libnss3=2:3.110-1+deb13u4 \
      libpcsclite1=2.3.3-1
  for deb in ./*.deb; do dpkg-deb -x "$deb" "$KT/qemu/"; done
)
QEMU_BIN="$KT/qemu/usr/bin/qemu-system-x86_64"
LD_LIBRARY_PATH="$KT/qemu/usr/lib/x86_64-linux-gnu" "$QEMU_BIN" --version | sed -n '1p'
# The firmware the pc machine loads at boot; kernel-test points QEMU
# at these relocated directories with -L.
for fw in usr/share/seabios/bios-256k.bin usr/share/qemu/linuxboot_dma.bin \
          usr/share/qemu/kvmvapic.bin; do
  test -s "$KT/qemu/$fw" || { printf 'FATAL: QEMU firmware missing: %s\n' "$fw" >&2; exit 1; }
done

# The pc machine reads its BIOS through the explicit firmware directory.
cp "$KT/qemu/usr/share/seabios/bios-256k.bin" "$KT/qemu/usr/share/qemu/"
curl --fail --location --silent --show-error --max-time 120 --retry 2 \
  --proto '=https' --proto-redir '=https' \
  -o "$KT/crane.tar.gz" \
  https://github.com/google/go-containerregistry/releases/download/v0.21.5/go-containerregistry_Linux_x86_64.tar.gz
printf '9f823ae5ee25803161110f957b5fd4538f714d40cdf25dacb4914fefafd246bf  %s/crane.tar.gz\n' "$KT" | sha256sum -c -
tar -xzf "$KT/crane.tar.gz" -C "$KT" crane
export DOCKER_CONFIG="$KT/anonymous"
mkdir -p "$DOCKER_CONFIG"
"$KT/crane" export europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-kernels@sha256:dfad86067b53473905cecf6fadd973112ed8606a93b0b3836693a77e10828fa3 "$KT/kernel.tar"
tar -xf "$KT/kernel.tar" -C "$KT/kernel" boot/vmlinuz boot/config
for option in CONFIG_BPF_LSM=y CONFIG_CGROUPS=y CONFIG_BLK_DEV_INITRD=y CONFIG_SECURITYFS=y CONFIG_FUNCTION_TRACER=y; do
  grep -qx "$option" "$KT/kernel/boot/config"
done
sha256sum "$KT/kernel/boot/vmlinuz"
