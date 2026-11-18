"""Tiny job broker for workers that cannot accept inbound connections
(a Kaggle/Colab notebook, a machine behind NAT or a firewall).

    agent  --POST frame-->  broker  <--long-poll--  worker (outbound only)
    agent  <--GET result--  broker  <--POST result-- worker

Real-time rules: jobs expire after `ttl` seconds and the queue is bounded, because a
fire alert about a 2-minute-old frame is useless. Oldest frames are dropped first.
Run it somewhere both sides can reach with outbound HTTPS (e.g. your AWS server).
State is in memory only; nothing is written to disk.
"""
from __future__ import annotations

import argparse
import base64
import hmac
import json
import os
import secrets
import threading
import time
import uuid
from collections import deque

from fastapi import Body, Depends, FastAPI, File, Form, Header, HTTPException, Query, Response, UploadFile


class Broker:
    def __init__(self, ttl: float = 20.0, max_queue: int = 50):
        self.ttl = ttl
        self.max_queue = max_queue
        self.cv = threading.Condition()
        self.queue: deque[str] = deque()
        self.jobs: dict[str, dict] = {}
        self.workers: dict[str, float] = {}
        self.dropped = 0

    def _expire(self, now: float):
        for jid in [j for j, v in self.jobs.items() if now - v["created"] > self.ttl * 3]:
            self.jobs.pop(jid, None)

    def submit(self, meta: dict, image: bytes) -> str:
        jid = uuid.uuid4().hex
        meta = dict(meta, job_id=jid)
        with self.cv:
            while len(self.queue) >= self.max_queue:
                old = self.queue.popleft()
                self.jobs.pop(old, None)
                self.dropped += 1
            self.jobs[jid] = {"meta": meta, "image": image, "created": time.time(), "result": None}
            self.queue.append(jid)
            self._expire(time.time())
            self.cv.notify_all()
        return jid

    def next(self, wait: float, worker: str):
        end = time.time() + wait
        with self.cv:
            self.workers[worker] = time.time()
            while True:
                now = time.time()
                while self.queue:
                    jid = self.queue.popleft()
                    job = self.jobs.get(jid)
                    if job and now - job["created"] <= self.ttl:
                        return jid, job
                    self.jobs.pop(jid, None)
                    self.dropped += 1
                left = end - now
                if left <= 0:
                    return None
                self.cv.wait(timeout=left)

    def complete(self, jid: str, result: dict) -> bool:
        with self.cv:
            job = self.jobs.get(jid)
            if not job:
                return False
            job["result"] = result
            self.cv.notify_all()
            return True

    def result(self, jid: str, wait: float):
        """returns (state, result): state in done|pending|gone"""
        end = time.time() + wait
        with self.cv:
            while True:
                job = self.jobs.get(jid)
                if job is None:
                    return "gone", None
                if job["result"] is not None:
                    self.jobs.pop(jid, None)
                    return "done", job["result"]
                left = end - time.time()
                if left <= 0 or time.time() - job["created"] > self.ttl:
                    return "pending", None
                self.cv.wait(timeout=min(left, 0.5))


def create_app(agent_token: str, worker_token: str, ttl: float = 20.0, max_queue: int = 50) -> FastAPI:
    broker = Broker(ttl, max_queue)
    app = FastAPI(title="Gavety job broker", docs_url=None, redoc_url=None)

    def _need(expected: str):
        def dep(authorization: str = Header(default="")):
            if not hmac.compare_digest(authorization, f"Bearer {expected}"):
                raise HTTPException(status_code=401, detail="bad token")
        return dep

    agent = Depends(_need(agent_token))
    worker = Depends(_need(worker_token))

    @app.get("/health")
    def health():
        return {"ok": True}

    @app.get("/v1/stats", dependencies=[agent])
    def stats():
        now = time.time()
        return {"queued": len(broker.queue), "dropped": broker.dropped,
                "workers_seen_last_60s": sorted(w for w, t in broker.workers.items() if now - t < 60)}

    # --- agent side ---
    @app.post("/v1/jobs", dependencies=[agent])
    def submit(meta: str = Form(...), image: UploadFile = File(...)):
        try:
            m = json.loads(meta)
        except json.JSONDecodeError:
            raise HTTPException(status_code=400, detail="meta must be JSON")
        return {"job_id": broker.submit(m, image.file.read())}

    @app.get("/v1/jobs/{job_id}/result", dependencies=[agent])
    def get_result(job_id: str, response: Response, wait: float = Query(5, ge=0, le=25)):
        state, res = broker.result(job_id, wait)
        if state == "done":
            return res
        if state == "gone":
            raise HTTPException(status_code=404, detail="job expired or unknown")
        response.status_code = 202
        return {"status": "pending"}

    # --- worker side ---
    @app.get("/v1/jobs/next", dependencies=[worker])
    def next_job(response: Response, wait: float = Query(20, ge=0, le=30), worker: str = Query("worker")):
        got = broker.next(wait, worker)
        if got is None:
            response.status_code = 204
            return None
        jid, job = got
        return {"job_id": jid, "meta": job["meta"], "image_b64": base64.b64encode(job["image"]).decode()}

    @app.post("/v1/jobs/{job_id}/result", dependencies=[worker])
    def post_result(job_id: str, result: dict = Body(...)):
        if not broker.complete(job_id, result):
            raise HTTPException(status_code=404, detail="job expired or unknown")
        return {"ok": True}

    return app


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=8900)
    ap.add_argument("--ttl", type=float, default=20.0, help="seconds before an unprocessed frame is dropped")
    a = ap.parse_args()
    agent_t = os.environ.get("GAVETY_AGENT_TOKEN") or secrets.token_urlsafe(24)
    worker_t = os.environ.get("GAVETY_WORKER_TOKEN") or secrets.token_urlsafe(24)
    print(f"agent token : {agent_t}\nworker token: {worker_t}")
    import uvicorn

    uvicorn.run(create_app(agent_t, worker_t, a.ttl), host=a.host, port=a.port)


if __name__ == "__main__":
    main()
