"""Pluggable inference engines for the Gavety AI worker.

An engine takes ONE jpeg frame and returns a list of detections:
    {"label": str, "conf": 0..1, "box": [x1,y1,x2,y2] normalised 0..1 or None, "source": engine name}

Engines are stateless on purpose: the Gavety agent owns all state (zones, N-of-M
confirmation, cooldowns). That is what lets any machine - a local GPU PC, a cloud VM,
a Kaggle notebook - act as a worker with no coordination between workers.
"""
from __future__ import annotations

import base64
import io
import json
from typing import Any


def _load_image(data: bytes):
    from PIL import Image

    return Image.open(io.BytesIO(data)).convert("RGB")


class Engine:
    name = "engine"

    def analyze(self, image_bytes: bytes, meta: dict) -> list[dict]:
        raise NotImplementedError


class MockEngine(Engine):
    """No model needed. Light-grey blobs -> 'person', red blobs -> 'fire'.
    Only for wiring tests and demos of the pipeline; it detects nothing real."""

    def __init__(self, name: str = "mock", **_: Any):
        self.name = name

    def analyze(self, image_bytes: bytes, meta: dict) -> list[dict]:
        import numpy as np

        img = np.asarray(_load_image(image_bytes))
        h, w, _ = img.shape
        r, g, b = img[:, :, 0], img[:, :, 1], img[:, :, 2]
        out = []
        for label, mask in (
            ("person", (r > 180) & (g > 180) & (b > 180)),
            ("fire", (r > 200) & (g < 60) & (b < 60)),
        ):
            ys, xs = np.nonzero(mask)
            if len(xs) >= 50:
                out.append({
                    "label": label,
                    "conf": 0.9,
                    "box": [round(xs.min() / w, 4), round(ys.min() / h, 4),
                            round((xs.max() + 1) / w, 4), round((ys.max() + 1) / h, 4)],
                    "source": self.name,
                })
        return out


class YoloEngine(Engine):
    """Ultralytics YOLO (.pt or .onnx). Runs on CUDA if available, else CPU.

    labels: keep only these class names (None = keep all).
    rename: map model class names to the labels the agent's rules use,
            e.g. {"Fire": "fire", "no-helmet": "no_helmet"}.
    """

    def __init__(self, name: str, model: str, device: str | int | None = None, conf: float = 0.35,
                 imgsz: int = 640, labels: list[str] | None = None, rename: dict | None = None, **_: Any):
        from ultralytics import YOLO  # imported late so the rest works without it

        self.name = name
        self.model = YOLO(model)
        self.device = device
        self.conf = conf
        self.imgsz = imgsz
        self.labels = {x.lower() for x in labels} if labels else None
        self.rename = {k.lower(): v for k, v in (rename or {}).items()}

    def analyze(self, image_bytes: bytes, meta: dict) -> list[dict]:
        img = _load_image(image_bytes)
        w, h = img.size
        res = self.model.predict(img, conf=self.conf, imgsz=self.imgsz, device=self.device, verbose=False)[0]
        out = []
        for b in res.boxes:
            raw = str(res.names[int(b.cls[0])])
            if self.labels is not None and raw.lower() not in self.labels:
                continue
            x1, y1, x2, y2 = b.xyxy[0].tolist()
            out.append({
                "label": self.rename.get(raw.lower(), raw),
                "conf": round(float(b.conf[0]), 4),
                "box": [round(x1 / w, 4), round(y1 / h, 4), round(x2 / w, 4), round(y2 / h, 4)],
                "source": self.name,
            })
        return out


class OllamaVLMEngine(Engine):
    """Asks a local vision-language model (via Ollama) a yes/no question per hazard.

    New hazard = add a word to `hazards` (or edit `prompt`); no training needed.
    Verdicts are NOT calibrated probabilities, and small models are often wrong -
    use this as a second opinion on frames a detector already flagged, not as the
    only trigger. Detections carry box=None.
    """

    def __init__(self, name: str, model: str = "qwen2.5vl:3b", url: str = "http://127.0.0.1:11434",
                 hazards: list[str] | None = None, prompt: str | None = None, max_side: int = 512,
                 timeout: float = 90, keep_alive: str = "5m", **_: Any):
        self.name = name
        self.model = model
        self.url = url.rstrip("/")
        self.hazards = hazards or ["fire", "smoke"]
        self.prompt = prompt
        self.max_side = max_side
        self.timeout = timeout
        self.keep_alive = keep_alive

    def _prompt(self) -> str:
        if self.prompt:
            return self.prompt
        keys = ", ".join(f'"{h}"' for h in self.hazards)
        return ("This is a frame from an industrial CCTV camera. For each hazard below, say whether it is "
                f"CLEARLY visible. Hazards: {keys}. Reply with JSON only, mapping each hazard to a number "
                "from 0 (not visible) to 1 (certainly visible). Do not guess; use 0 when unsure.")

    def analyze(self, image_bytes: bytes, meta: dict) -> list[dict]:
        import requests

        img = _load_image(image_bytes)
        img.thumbnail((self.max_side, self.max_side))
        buf = io.BytesIO()
        img.save(buf, "JPEG", quality=85)
        payload = {
            "model": self.model,
            "stream": False,
            "format": "json",
            "keep_alive": self.keep_alive,
            "options": {"temperature": 0, "num_ctx": 2048},
            "messages": [{"role": "user", "content": self._prompt(),
                          "images": [base64.b64encode(buf.getvalue()).decode()]}],
        }
        r = requests.post(self.url + "/api/chat", json=payload, timeout=self.timeout)
        r.raise_for_status()
        verdict = json.loads(r.json()["message"]["content"])
        out = []
        for h in self.hazards:
            v = verdict.get(h, 0)
            if isinstance(v, bool):
                conf = 0.7 if v else 0.0
            else:
                try:
                    conf = max(0.0, min(1.0, float(v)))
                except (TypeError, ValueError):
                    conf = 0.0
            if conf > 0:
                out.append({"label": h, "conf": round(conf, 3), "box": None, "source": self.name})
        return out


ENGINE_TYPES = {"mock": MockEngine, "yolo": YoloEngine, "ollama": OllamaVLMEngine}


def build_engines(cfg: dict) -> dict[str, Engine]:
    """cfg = {"engines": [{"name": "fire", "type": "yolo", "model": "...", ...}, ...]}"""
    engines: dict[str, Engine] = {}
    for spec in cfg.get("engines", []):
        spec = dict(spec)
        kind = spec.pop("type")
        if kind not in ENGINE_TYPES:
            raise ValueError(f"unknown engine type {kind!r}; choose from {sorted(ENGINE_TYPES)}")
        engines[spec["name"]] = ENGINE_TYPES[kind](**spec)
    return engines
