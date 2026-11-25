#!/bin/bash
# Usage: bash asterisk/tests/run-smoke.sh ASTERISK_IMAGE PYTHON_RUNNER_IMAGE
set -euo pipefail
image="${1:?provide the Asterisk image to test}"
runner="${2:-python:3.12-slim}"
directory="$(cd -- "$(dirname -- "$0")" && pwd)"
name="prismvox-asterisk-smoke-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
for mode in tls udp; do
    docker run --detach --name "$name" --network none \
        -e ARI_USER=fixture -e ARI_PASS=fixture-password \
        -e SIP_USERNAME=fixture -e SIP_PASSWORD=fixture-password \
        -e SIP_REGISTRAR=127.0.0.1 -e SIP_REGISTRAR_IP=127.0.0.1 \
        -e SIP_DOMAIN=fixture.invalid -e SIP_TLS_PORT=15061 -e SIP_UDP_PORT=15060 \
        -e SIP_TRANSPORT_MODE="$mode" "$image" >/dev/null
    if ! docker run --rm --network "container:$name" \
        -v "$directory/smoke.py:/smoke.py:ro" --entrypoint python3 "$runner" /smoke.py; then
        docker logs "$name"
        exit 1
    fi
    transports="$(docker exec "$name" asterisk -rx 'pjsip show transports')"
    endpoint="$(docker exec "$name" asterisk -rx 'pjsip show endpoint phoneagent-endpoint')"
    tls="$(docker exec "$name" asterisk -rx 'pjsip show transport transport-tls')"
    transfer="$(docker exec "$name" asterisk -rx 'dialplan show transfer')"
    inbound="$(docker exec "$name" asterisk -rx 'dialplan show from-trunk')"
    grep -Eq 'transport-udp[[:space:]]+udp' <<< "$transports"
    grep -Eq 'transport-tls[[:space:]]+tls' <<< "$transports"
    grep -Eq "transport[[:space:]]+:[[:space:]]+transport-$mode" <<< "$endpoint"
    grep -Eq 'allow[[:space:]]+:[[:space:]]+\(g722\|alaw\|ulaw\)' <<< "$endpoint"
    encryption=no
    [ "$mode" != tls ] || encryption=sdes
    grep -Eq "media_encryption[[:space:]]+:[[:space:]]+$encryption" <<< "$endpoint"
    grep -Eiq 'verify_server[[:space:]]+:[[:space:]]+yes' <<< "$tls"
    grep -Fq 'Dial(PJSIP/${EXTEN}@phoneagent-endpoint)' <<< "$transfer"
    grep -Fq 'Stasis(phoneagent)' <<< "$inbound"
    printf 'PJSIP %s: transports, TLS certificate verification, G.722/G.711 codecs and inbound/transfer dialplans passed\n' "$mode"
    docker rm -f "$name" >/dev/null
done
