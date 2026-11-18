"""Direct-mode worker: the Gavety agent POSTs a frame, gets detections back.

Run it on any machine that has your GPU (laptop, new PC, a cloud VM):
    python -m gavety_worker.server --config config.json --port 8800
"""
from __future__ import annotations

import argparse
import hmac
import json
import os
import secrets
import threading

from fastapi import Depends, FastAPI, File, Form, Header, HTTPException, UploadFile

from .core import AllEnginesFailed, run_job
from .engines import build_engines


def create_app(engines: dict, token: str, worker_id: str = "worker", max_concurrency: int = 1) -> FastAPI:
    app = FastAPI(title="Gavety AI worker", docs_url=None, redoc_url=None)
    gate = threading.BoundedSemaphore(max_concurrency)  # protects a small GPU from parallel overload

    def auth(authorization: str = Header(default="")):
        if not hmac.compare_digest(authorization, f"Bearer {token}"):
            raise HTTPException(status_code=401, detail="bad token")

    @app.get("/health")
    def health():
        return {"ok": True, "worker": worker_id, "engines": sorted(engines)}

    @app.post("/v1/analyze", dependencies=[Depends(auth)])
    def analyze(meta: str = Form(...), image: UploadFile = File(...)):
        try:
            meta_d = json.loads(meta)
        except json.JSONDecodeError:
            raise HTTPException(status_code=400, detail="meta must be JSON")
        data = image.file.read()
        with gate:
            try:
                return run_job(engines, meta_d, data, worker_id)
            except AllEnginesFailed as e:
                raise HTTPException(status_code=503, detail=str(e))

    return app


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default="config.json")
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=8800)
    ap.add_argument("--worker-id", default=os.environ.get("GAVETY_WORKER_ID", "worker"))
    a = ap.parse_args()

    with open(a.config) as f:
        cfg = json.load(f)
    token = os.environ.get("GAVETY_TOKEN")
    if not token:
        token = secrets.token_urlsafe(24)
        print(f"GAVETY_TOKEN not set - generated one for this run:\n  {token}")
    import uvicorn

    uvicorn.run(create_app(build_engines(cfg), token, a.worker_id), host=a.host, port=a.port)


if __name__ == "__main__":
    main()
