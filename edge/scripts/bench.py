"""Time the same Kiln calls directly against the gateway and through the Cloudflare Worker.

    gateway/.venv/bin/python edge/scripts/bench.py --edge https://kiln-edge.<you>.workers.dev -n 30

The direct path uses the edge secret (as the Worker does); the edge path uses the client API key.
Both are read from ~/.config/kiln and never printed. Results go to bench/results/edge-<time>.json.
"""
import argparse
import json
import pathlib
import statistics
import time

import httpx

CONF = pathlib.Path.home() / ".config" / "kiln"


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, round(p / 100 * (len(xs) - 1)))]


def run(base, headers, n):
    with httpx.Client(base_url=base, headers=headers, timeout=60) as c:
        t = time.perf_counter()
        r = c.post("/v1/sandboxes", json={})
        create_ms = (time.perf_counter() - t) * 1000
        r.raise_for_status()
        sid = r.json()["id"]
        try:
            for _ in range(3):  # warm up connections and the sandbox
                c.post(f"/v1/sandboxes/{sid}/exec", json={"argv": ["python3", "-c", "pass"]}).raise_for_status()
            lat, timing = [], []
            for _ in range(n):
                t = time.perf_counter()
                r = c.post(f"/v1/sandboxes/{sid}/exec", json={"argv": ["python3", "-c", "print(6*7)"]})
                lat.append((time.perf_counter() - t) * 1000)
                r.raise_for_status()
                assert r.json()["stdout"].strip() == "42", r.text
                if "server-timing" in r.headers:
                    timing.append(r.headers["server-timing"])
        finally:
            c.delete(f"/v1/sandboxes/{sid}")
    return {"create_ms": round(create_ms, 1), "n": n, "p50_ms": round(pct(lat, 50), 1),
            "p95_ms": round(pct(lat, 95), 1), "mean_ms": round(statistics.mean(lat), 1),
            "server_timing_sample": timing[-1] if timing else None}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--direct", default="http://127.0.0.1:8080")
    ap.add_argument("--edge", required=True)
    ap.add_argument("-n", type=int, default=30)
    a = ap.parse_args()
    secret = (CONF / "edge-secret").read_text().strip()
    key = (CONF / "api-key").read_text().strip()
    out = {
        "when": time.strftime("%Y-%m-%dT%H:%M:%S"),
        "direct": run(a.direct, {"x-kiln-edge-secret": secret}, a.n),
        "edge": run(a.edge, {"authorization": f"Bearer {key}"}, a.n),
    }
    path = pathlib.Path(__file__).resolve().parents[2] / "bench" / "results" / f"edge-{time.strftime('%Y%m%d-%H%M%S')}.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(out, indent=2))
    print(json.dumps(out, indent=2))
    print(f"saved {path}")


if __name__ == "__main__":
    main()
