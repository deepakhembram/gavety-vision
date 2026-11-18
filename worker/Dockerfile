# GPU worker. CUDA 12.1 build of PyTorch on purpose: newer CUDA builds are dropping
# support for older NVIDIA cards (Pascal, e.g. GTX 1050). Untested here (no Docker daemon
# in the build environment) - first `docker compose build` is the real test.
FROM pytorch/pytorch:2.4.1-cuda12.1-cudnn9-runtime
RUN apt-get update && apt-get install -y --no-install-recommends libgl1 libglib2.0-0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt ultralytics
COPY gavety_worker ./gavety_worker
EXPOSE 8800
CMD ["python", "-m", "gavety_worker.server", "--config", "/app/config.json", "--port", "8800"]
