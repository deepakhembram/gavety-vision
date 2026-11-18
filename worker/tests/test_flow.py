import io
import json
import socket
import threading
import time

import numpy as np
import pytest
import requests
import uvicorn
from PIL import Image

from gavety_worker import broker as broker_mod
from gavety_worker import server as server_mod
from gavety_worker.core import AllEnginesFailed, run_job
from gavety_worker.engines import Engine, OllamaVLMEngine, build_engines
from gavety_worker.pull_worker import pull_loop


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


class Srv:
    def __init__(self, app):
        self.port = free_port()
        self.server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=self.port, log_level="error"))
        self.t = threading.Thread(target=self.server.run, daemon=True)
        self.t.start()
        for _ in range(100):
            try:
                requests.get(self.url + "/health", timeout=0.2)
                break
            except requests.RequestException:
                time.sleep(0.05)

    @property
    def url(self):
        return f"http://127.0.0.1:{self.port}"

    def stop(self):
        self.server.should_exit = True
        self.t.join(timeout=3)


def frame(person_x=None, fire=False, w=640, h=360):
    a = np.full((h, w, 3), 60, np.uint8)
    if person_x is not None:
        a[150:300, person_x:person_x + 40] = 220
    if fire:
        a[80:130, 80:130] = (255, 0, 0)
    buf = io.BytesIO()
    Image.fromarray(a).save(buf, "JPEG", quality=90)
    return buf.getvalue()


ENGINES = build_engines({"engines": [{"name": "person", "type": "mock"}, {"name": "fire", "type": "mock"}]})


def test_run_job_selects_tasks_and_normalises_boxes():
    res = run_job(ENGINES, {"camera": "c1", "tasks": ["person"]}, frame(person_x=400, fire=True))
    labels = [d["label"] for d in res["detections"]]
    assert "person" in labels  # fire blob is also red-ish but 'fire' engine was not requested...
    assert res["model"] == "person"
    box = next(d for d in res["detections"] if d["label"] == "person")["box"]
    assert all(0 <= v <= 1 for v in box) and 0.6 < box[0] < 0.65


def test_unknown_engine_only_is_failure():
    with pytest.raises(AllEnginesFailed):
        run_job(ENGINES, {"tasks": ["nope"]}, frame())


def test_direct_worker_auth_and_analyze():
    srv = Srv(server_mod.create_app(ENGINES, "secret", "w1"))
    try:
        meta = json.dumps({"camera": "c1", "tasks": ["person", "fire"]})
        files = {"image": ("f.jpg", frame(person_x=400, fire=True), "image/jpeg")}
        assert requests.post(srv.url + "/v1/analyze", data={"meta": meta}, files=files).status_code == 401
        r = requests.post(srv.url + "/v1/analyze", data={"meta": meta}, files=files,
                          headers={"Authorization": "Bearer secret"})
        assert r.status_code == 200
        body = r.json()
        assert {d["label"] for d in body["detections"]} >= {"person", "fire"}
        assert body["worker"] == "w1"
        bad = requests.post(srv.url + "/v1/analyze", data={"meta": json.dumps({"tasks": ["zzz"]})}, files=files,
                            headers={"Authorization": "Bearer secret"})
        assert bad.status_code == 503  # lets the agent fall back to another provider
    finally:
        srv.stop()


def test_broker_pull_worker_roundtrip():
    hub = Srv(broker_mod.create_app("agent-t", "worker-t", ttl=10))
    stop = threading.Event()
    done = []
    t = threading.Thread(
        target=lambda: done.append(pull_loop(hub.url, "worker-t", ENGINES, "kaggle-1", wait=2, stop=stop.is_set)),
        daemon=True)
    t.start()
    try:
        H = {"Authorization": "Bearer agent-t"}
        # wrong token rejected on both sides
        assert requests.get(hub.url + "/v1/stats").status_code == 401
        assert requests.get(hub.url + "/v1/jobs/next", params={"wait": 0}, headers=H).status_code == 401

        meta = json.dumps({"camera": "c9", "tasks": ["fire"]})
        t0 = time.time()
        jid = requests.post(hub.url + "/v1/jobs", data={"meta": meta}, headers=H,
                            files={"image": ("f.jpg", frame(fire=True), "image/jpeg")}).json()["job_id"]
        res = requests.get(hub.url + f"/v1/jobs/{jid}/result", params={"wait": 5}, headers=H)
        assert res.status_code == 200, res.text
        body = res.json()
        assert body["worker"] == "kaggle-1" and body["detections"][0]["label"] == "fire"
        assert time.time() - t0 < 3
        # result is consumed once
        assert requests.get(hub.url + f"/v1/jobs/{jid}/result", params={"wait": 0}, headers=H).status_code == 404
        assert "kaggle-1" in requests.get(hub.url + "/v1/stats", headers=H).json()["workers_seen_last_60s"]
    finally:
        stop.set()
        hub.stop()


def test_broker_drops_stale_and_bounds_queue():
    b = broker_mod.Broker(ttl=0.2, max_queue=3)
    ids = [b.submit({"n": i}, b"x") for i in range(5)]
    assert len(b.queue) == 3 and b.dropped == 2  # oldest two dropped
    time.sleep(0.3)
    assert b.next(0, "w") is None  # everything left is stale -> never processed
    assert b.dropped == 5


def test_pending_result_returns_202_when_no_worker():
    hub = Srv(broker_mod.create_app("a", "w", ttl=10))
    try:
        H = {"Authorization": "Bearer a"}
        jid = requests.post(hub.url + "/v1/jobs", data={"meta": "{}"}, headers=H,
                            files={"image": ("f.jpg", frame(), "image/jpeg")}).json()["job_id"]
        assert requests.get(hub.url + f"/v1/jobs/{jid}/result", params={"wait": 0.2}, headers=H).status_code == 202
    finally:
        hub.stop()


def test_ollama_engine_parses_verdicts(monkeypatch):
    seen = {}

    def fake_post(url, json=None, timeout=None):
        seen["url"], seen["payload"] = url, json

        class R:
            def raise_for_status(self): pass
            def json(self): return {"message": {"content": '{"fire": 0.8, "smoke": false, "sparks": "x"}'}}
        return R()

    monkeypatch.setattr(requests, "post", fake_post)
    eng = OllamaVLMEngine("vlm", hazards=["fire", "smoke", "sparks"], url="http://h:11434/")
    out = eng.analyze(frame(fire=True), {})
    assert [(d["label"], d["conf"]) for d in out] == [("fire", 0.8)]  # false / garbage -> dropped
    assert seen["url"] == "http://h:11434/api/chat"
    assert seen["payload"]["options"]["temperature"] == 0 and len(seen["payload"]["messages"][0]["images"]) == 1
