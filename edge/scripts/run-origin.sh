#!/usr/bin/env bash
# Start the origin behind the Worker: kilnd (Firecracker), the REST gateway with the edge-secret
# check on, and a Cloudflare quick tunnel to the gateway. Prints the tunnel URL to deploy with.
#
#   edge/scripts/run-origin.sh            # from the repo root; Ctrl-C stops everything
#
# Secrets are read from ~/.config/kiln/edge-secret (never printed).
set -euo pipefail
cd "$(dirname "$0")/../.."
LOG=${LOG:-/tmp/kiln-origin}
KILN_LISTEN=${KILN_LISTEN:-127.0.0.1:7171}   # 7070 is often taken (e.g. by AnyDesk)
mkdir -p "$LOG"
trap 'kill 0' EXIT

bin/kilnd -listen "$KILN_LISTEN" -firecracker .tools/bin/firecracker -pools firecracker/python=4 > "$LOG/kilnd.log" 2>&1 &
for _ in $(seq 1 60); do grep -q "listening" "$LOG/kilnd.log" 2>/dev/null && break; sleep 1; done

KILN_EDGE_SECRET="$(cat ~/.config/kiln/edge-secret)" KILN_ADDR="$KILN_LISTEN" \
  gateway/.venv/bin/uvicorn app:app --app-dir gateway --host 127.0.0.1 --port 8080 > "$LOG/gateway.log" 2>&1 &

cloudflared tunnel --no-autoupdate --url http://127.0.0.1:8080 > "$LOG/tunnel.log" 2>&1 &
for _ in $(seq 1 60); do
  url=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$LOG/tunnel.log" | head -1 || true)
  [ -n "$url" ] && break
  sleep 1
done
echo "tunnel: ${url:-not ready, see $LOG/tunnel.log}"
echo "$url" > "$LOG/tunnel-url"
wait
