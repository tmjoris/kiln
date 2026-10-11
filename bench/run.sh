#!/usr/bin/env bash
# Runs the full benchmark suite for both backends inside a privileged
# container that plays the production host: root, /dev/kvm, runsc.
#   bench/run.sh [kiln-bench flags...]
# Needs: make build image, and .tools/ populated (see README).
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=bench/results/$(date -u +%Y%m%d-%H%M%S)
mkdir -p "$OUT"
docker run --rm --privileged --device /dev/kvm \
  -v "$PWD:/k" -w /k -e OUT="$OUT" -e HOST_UID="$(id -u):$(id -g)" \
  --name kiln-bench debian:bookworm-slim bash -euc '
    export PATH=/k/bin:/k/.tools/bin:$PATH
    kilnd -backends firecracker,gvisor -firecracker /k/.tools/bin/firecracker \
      -runsc /k/.tools/gvisor/runsc -gvisor-ignore-cgroups -data-dir /var/lib/kiln \
      -pools firecracker/python=4,gvisor/python=4 -min-mem-available-mib 2048 \
      > "$OUT/kilnd.log" 2>&1 &
    for i in $(seq 1 100); do kiln stats >/dev/null 2>&1 && break; sleep 0.1; done
    sleep 5   # let pools fill
    for b in firecracker gvisor; do
      kiln-bench -addr localhost:7070 -backend $b -out "$OUT" "$@" all 2>&1 | tee -a "$OUT/bench.log"
    done
    kill %1; wait || true
    chown -R "$HOST_UID" "$OUT"
  ' bash "$@"
echo "results in $OUT"
