"""Shared job runner used by both the HTTP worker and the pull worker."""
from __future__ import annotations

import time

from .engines import Engine


class AllEnginesFailed(Exception):
    pass


def run_job(engines: dict[str, Engine], meta: dict, image: bytes, worker_id: str = "worker") -> dict:
    """Run the engines named in meta['tasks'] (default: all) on one frame."""
    tasks = [t for t in (meta.get("tasks") or list(engines))]
    t0 = time.perf_counter()
    detections: list[dict] = []
    ran: list[str] = []
    errors: list[str] = []
    for t in tasks:
        eng = engines.get(t)
        if eng is None:
            errors.append(f"{t}: no such engine on this worker")
            continue
        try:
            detections += eng.analyze(image, meta)
            ran.append(t)
        except Exception as e:  # one bad engine must not hide the others' results
            errors.append(f"{t}: {type(e).__name__}: {e}")
    if tasks and not ran:
        raise AllEnginesFailed("; ".join(errors) or "no engines ran")
    return {
        "job_id": meta.get("job_id"),
        "camera": meta.get("camera"),
        "ts": meta.get("ts"),
        "worker": worker_id,
        "model": ",".join(ran),
        "latency_ms": int((time.perf_counter() - t0) * 1000),
        "detections": detections,
        "errors": errors,
    }
