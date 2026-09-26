FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/contextual-ad-lab .

# Pinned ONNX models (CLIP ViT-B/32 image encoder, YAMNet), verified by SHA-256 while downloading.
FROM python:3.12-slim-bookworm AS models
COPY ai/fetch_models.py /tmp/fetch_models.py
RUN python /tmp/fetch_models.py /models

FROM python:3.12-slim-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system app \
    && useradd --system --gid app --home-dir /app app \
    && mkdir -p /app/data/uploads \
    && chown -R app:app /app
COPY ai/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --no-deps -r /tmp/requirements.txt && rm /tmp/requirements.txt
WORKDIR /app
COPY --from=models --chown=app:app /models ./models
COPY --from=build --chown=app:app /out/contextual-ad-lab ./contextual-ad-lab
COPY --chown=app:app ai/ ./ai/
COPY --chown=app:app assets/ ./assets/
ENV ADDR=:8080
ENV UPLOAD_DIR=/app/data/uploads
ENV PYTHON_BIN=python3
ENV AI_WORKER_PATH=/app/ai/worker.py
ENV AI_BRANDS_PATH=/app/assets/brands.json
ENV MODELS_DIR=/app/models
ENV PYTHONDONTWRITEBYTECODE=1
EXPOSE 8080
USER app
ENTRYPOINT ["/app/contextual-ad-lab"]
