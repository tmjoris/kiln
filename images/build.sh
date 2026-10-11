#!/usr/bin/env bash
# Builds guest artifacts into build/: rootfs.tar (gVisor), rootfs.ext4
# (Firecracker) and vmlinux. Needs only docker, not root.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=build/images/python
KERNEL_URL=${KERNEL_URL:-https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155}
mkdir -p "$OUT" build/kernel

docker build -q -f images/rootfs.Dockerfile -t kiln-rootfs:python . >/dev/null
cid=$(docker create kiln-rootfs:python)
trap 'docker rm -f "$cid" >/dev/null' EXIT
docker export "$cid" > "$OUT/rootfs.tar"

# Extract and populate as root inside a throwaway container so file
# ownership survives (mke2fs < 1.47.1 cannot read tarballs directly).
size_mb=$(( $(stat -c %s "$OUT/rootfs.tar") / 1048576 * 13 / 10 + 64 ))
docker run --rm -v "$PWD/$OUT:/out" debian:bookworm-slim sh -euc "
  apt-get -qq update >/dev/null && apt-get -qq install -y e2fsprogs >/dev/null
  mkdir /r && tar -C /r -xf /out/rootfs.tar
  rm -f /out/rootfs.ext4
  mkfs.ext4 -q -F -O ^has_journal -L kiln-root -d /r /out/rootfs.ext4 ${size_mb}M
  chown $(id -u):$(id -g) /out/rootfs.ext4"

[ -f build/kernel/vmlinux ] || curl -sSfL -o build/kernel/vmlinux "$KERNEL_URL"
ls -lh "$OUT" build/kernel
