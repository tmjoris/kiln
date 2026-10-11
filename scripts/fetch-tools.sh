#!/usr/bin/env bash
# Downloads pinned, checksum-verified tools into .tools/ (no root needed).
set -euo pipefail
cd "$(dirname "$0")/.."
FC=v1.17.0 FC_SHA=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
GV=20261005.0 GV_SHA=bd4eb2ca0ee74f0fbbb5f5273ba011e79c56a29fbb047393b88b981cd3230ede
PROTOC=36.2 TF=1.16.5
mkdir -p .tools/bin .tools/dl
cd .tools/dl

curl -fsSL -o fc.tgz "https://github.com/firecracker-microvm/firecracker/releases/download/$FC/firecracker-$FC-x86_64.tgz"
echo "$FC_SHA  fc.tgz" | sha256sum -c -
tar xzf fc.tgz
install -m 0755 "release-$FC-x86_64/firecracker-$FC-x86_64" ../bin/firecracker
install -m 0755 "release-$FC-x86_64/jailer-$FC-x86_64" ../bin/jailer

curl -fsSL -o gvisor.tar.bz2 "https://github.com/google/gvisor/releases/download/release-$GV/gvisor-x86_64.tar.bz2"
echo "$GV_SHA  gvisor.tar.bz2" | sha256sum -c -
rm -rf ../gvisor && mkdir -p ../gvisor && tar xjf gvisor.tar.bz2 -C ../gvisor
ln -sf ../gvisor/runsc ../bin/runsc

curl -fsSL -o protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v$PROTOC/protoc-$PROTOC-linux-x86_64.zip"
rm -rf ../protoc && python3 -I -m zipfile -e protoc.zip ../protoc && chmod +x ../protoc/bin/protoc
ln -sf ../protoc/bin/protoc ../bin/protoc

curl -fsSL -o tf.zip "https://releases.hashicorp.com/terraform/$TF/terraform_${TF}_linux_amd64.zip"
python3 -I -m zipfile -e tf.zip tf && install -m 0755 tf/terraform ../bin/terraform

../bin/firecracker --version | head -1
../bin/runsc --version | head -1
