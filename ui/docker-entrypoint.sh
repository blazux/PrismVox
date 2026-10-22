#!/bin/sh
# Browsers only expose navigator.mediaDevices (and thus getUserMedia, needed by
# the voice-cloning "Record" button) in a secure context: https:, or http: on
# localhost/127.0.0.1. This deployment is reached over plain HTTP by IP, which
# is neither — so serve HTTPS too, on a second port, with a self-signed cert
# generated on first boot and kept in a mounted volume (regenerating it on
# every restart would make the browser re-warn about a "new" certificate each
# time). Plain HTTP stays on 7860 unchanged, for existing integrations
# (N8N webhooks, AGENT_API_KEY callers) that don't care about the browser mic.
set -eu

CERT_DIR=/certs
HTTPS_PORT="${UI_HTTPS_PORT:-7861}"

mkdir -p "$CERT_DIR"
if [ ! -s "$CERT_DIR/cert.pem" ] || [ ! -s "$CERT_DIR/key.pem" ]; then
    echo "[entrypoint] generating a self-signed TLS cert for the admin UI (first boot)…"
    SAN="DNS:localhost,IP:127.0.0.1"
    if [ -n "${UI_TLS_SAN:-}" ]; then
        SAN="$SAN,$UI_TLS_SAN"
    fi
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout "$CERT_DIR/key.pem" -out "$CERT_DIR/cert.pem" \
        -subj "/CN=prismvox-ui" -addext "subjectAltName=$SAN"
fi

uvicorn main:app --host 0.0.0.0 --port 7860 &
HTTP_PID=$!
uvicorn main:app --host 0.0.0.0 --port "$HTTPS_PORT" \
    --ssl-keyfile "$CERT_DIR/key.pem" --ssl-certfile "$CERT_DIR/cert.pem" &
HTTPS_PID=$!

trap 'kill $HTTP_PID $HTTPS_PID 2>/dev/null || true' TERM INT
wait $HTTP_PID $HTTPS_PID
