FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/contextual-ad-lab .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates ffmpeg python3 \
    && addgroup -S app \
    && adduser -S -G app app \
    && mkdir -p /app/data/uploads \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build --chown=app:app /out/contextual-ad-lab ./contextual-ad-lab
COPY --chown=app:app ai/ ./ai/
COPY --chown=app:app assets/brands.json ./assets/brands.json
ENV ADDR=:8080
ENV UPLOAD_DIR=/app/data/uploads
ENV PYTHON_BIN=python3
ENV AI_WORKER_PATH=/app/ai/worker.py
ENV AI_BRANDS_PATH=/app/assets/brands.json
EXPOSE 8080
USER app
ENTRYPOINT ["/app/contextual-ad-lab"]
