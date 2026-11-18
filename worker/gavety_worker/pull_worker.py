"""Pull-mode worker: connects OUT to a broker, so it works behind NAT / in a notebook.

    python -m gavety_worker.pull_worker --hub https://hub.example.com --config config.json
    (set GAVETY_WORKER_TOKEN in the environment)
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import time

import requests

from .core import AllEnginesFailed, run_job
from .engines import build_engines


def pull_loop(hub: str, token: str, engines: dict, worker_id: str = "pull-worker", wait: float = 20,
              max_jobs: int | None = None, stop=lambda: False) -> int:
    """Returns number of jobs handled. max_jobs/stop exist so tests can end the loop."""
    hub = hub.rstrip("/")
    headers = {"Authorization": f"Bearer {token}"}
    s = requests.Session()
    done = 0
    backoff = 1.0
    while not stop() and (max_jobs is None or done < max_jobs):
        try:
            r = s.get(f"{hub}/v1/jobs/next", params={"wait": wait, "worker": worker_id},
                      headers=headers, timeout=wait + 10)
            if r.status_code == 204:
                backoff = 1.0
                continue
            r.raise_for_status()
            job = r.json()
            try:
                result = run_job(engines, job["meta"], base64.b64decode(job["image_b64"]), worker_id)
            except AllEnginesFailed as e:
                result = {"job_id": job["job_id"], "worker": worker_id, "error": str(e), "detections": []}
            s.post(f"{hub}/v1/jobs/{job['job_id']}/result", json=result, headers=headers, timeout=15)
            done += 1
            backoff = 1.0
        except requests.RequestException as e:
            print(f"[pull-worker] hub unreachable ({e.__class__.__name__}); retry in {backoff:.0f}s")
            time.sleep(backoff)
            backoff = min(backoff * 2, 30)
    return done


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hub", required=True)
    ap.add_argument("--config", default="config.json")
    ap.add_argument("--worker-id", default=os.environ.get("GAVETY_WORKER_ID", "pull-worker"))
    a = ap.parse_args()
    token = os.environ.get("GAVETY_WORKER_TOKEN")
    if not token:
        raise SystemExit("set GAVETY_WORKER_TOKEN")
    with open(a.config) as f:
        engines = build_engines(json.load(f))
    print(f"[pull-worker] {a.worker_id} serving {sorted(engines)} from {a.hub}")
    pull_loop(a.hub, token, engines, a.worker_id)


if __name__ == "__main__":
    main()
