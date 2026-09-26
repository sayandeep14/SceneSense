#!/bin/sh
/app/scenesense-observer "${UPLOAD_DIR:-/app/data/uploads}/observability.json" &
exec /app/contextual-ad-lab
