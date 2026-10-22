#!/bin/bash
set -e
umask 077

for f in /etc/asterisk-tpl/*.conf; do
    cp "$f" "/etc/asterisk/$(basename "$f")"
done

export SIP_TLS_PORT="${SIP_TLS_PORT:-5061}"
export SIP_UDP_PORT="${SIP_UDP_PORT:-5060}"
export SIP_REGISTRAR_MATCH="${SIP_REGISTRAR_MATCH:-$SIP_REGISTRAR_IP}"

# sip_transport_mode: "tls" (default, SRTP over TLS — required by BroadWorks) or
# "udp" (plain SIP/RTP, no encryption). Derives the three placeholders the trunk
# template actually uses (SIP_TRANSPORT, SIP_MEDIA_ENCRYPTION_LINE, SIP_PORT) so
# the template itself stays mode-agnostic — see pjsip_trunk.conf.tpl.
if [ "${SIP_TRANSPORT_MODE:-tls}" = "udp" ]; then
    export SIP_TRANSPORT="transport-udp"
    export SIP_MEDIA_ENCRYPTION_LINE=""
    export SIP_PORT="$SIP_UDP_PORT"
else
    export SIP_TRANSPORT="transport-tls"
    export SIP_MEDIA_ENCRYPTION_LINE="media_encryption = sdes"
    export SIP_PORT="$SIP_TLS_PORT"
fi

envsubst '${ARI_USER} ${ARI_PASS}' \
    < /etc/asterisk-tpl/ari.conf.tpl \
    > /etc/asterisk/ari.conf

# New installs verify TLS certificates. Existing trunks keep their former choice
# until the operator explicitly opts in, so an unknown private CA cannot cut calls.
DYN=/etc/asterisk/dynamic
mkdir -p "$DYN"
if [ -z "${SIP_TLS_VERIFY_SERVER:-}" ]; then
    if [ -s "$DYN/tls_verify_server" ]; then
        SIP_TLS_VERIFY_SERVER="$(cat "$DYN/tls_verify_server")"
    elif [ -s "$DYN/pjsip_trunk.conf" ]; then
        SIP_TLS_VERIFY_SERVER=no
    else
        SIP_TLS_VERIFY_SERVER=yes
    fi
fi
case "$SIP_TLS_VERIFY_SERVER" in
    yes|true|1) SIP_TLS_VERIFY_SERVER=yes ;;
    no|false|0) SIP_TLS_VERIFY_SERVER=no ;;
    *) echo "Invalid SIP_TLS_VERIFY_SERVER: use yes or no" >&2; exit 1 ;;
esac
export SIP_TLS_VERIFY_SERVER
export SIP_TLS_CA_FILE="${SIP_TLS_CA_FILE:-/etc/ssl/certs/ca-certificates.crt}"
if [ "$SIP_TLS_VERIFY_SERVER" = yes ] && [ ! -r "$SIP_TLS_CA_FILE" ]; then
    echo "SIP TLS CA file is not readable" >&2; exit 1
fi
printf '%s\n' "$SIP_TLS_VERIFY_SERVER" > "$DYN/tls_verify_server"
if [ "$SIP_TLS_VERIFY_SERVER" = no ]; then
    echo "WARNING: SIP server certificate verification disabled (legacy trunk). Configure its CA and SIP_TLS_VERIFY_SERVER=yes." >&2
fi
envsubst '${SIP_TLS_VERIFY_SERVER} ${SIP_TLS_CA_FILE}' < /etc/asterisk-tpl/pjsip.conf.tpl > /etc/asterisk/pjsip.conf

# The SIP trunk lives in a shared volume so the UI can re-render + reload it at runtime.
DYN=/etc/asterisk/dynamic
mkdir -p "$DYN"
# Publish the canonical template for the UI renderer (always refresh from the image).
cp /etc/asterisk-tpl/pjsip_trunk.conf.tpl "$DYN/pjsip_trunk.conf.tpl"
# Bootstrap the rendered trunk from env on first boot only; afterwards the UI owns it.
if [ ! -s "$DYN/pjsip_trunk.conf" ]; then
    echo "Seeding SIP trunk from environment (first boot)…"
    envsubst '${SIP_REGISTRAR} ${SIP_REGISTRAR_IP} ${SIP_REGISTRAR_MATCH} ${SIP_USERNAME} ${SIP_PASSWORD} ${SIP_DOMAIN} ${SIP_TRANSPORT} ${SIP_MEDIA_ENCRYPTION_LINE} ${SIP_PORT}' \
        < /etc/asterisk-tpl/pjsip_trunk.conf.tpl \
        > "$DYN/pjsip_trunk.conf"
fi

mkdir -p /usr/share/asterisk/sounds/custom

# DEBUG=true → enable the PJSIP SIP packet logger once Asterisk is up, so every SIP
# message (and the server's response codes: 403, 407, 488…) is printed to the logs.
if [ "${DEBUG:-false}" = "true" ]; then
    echo "DEBUG=true → activation du logger SIP PJSIP au démarrage"
    (
        for _ in $(seq 1 30); do
            if asterisk -rx "core waitfullybooted" >/dev/null 2>&1; then
                asterisk -rx "pjsip set logger on"   >/dev/null 2>&1 || true
                asterisk -rx "logger set level DEBUG on" >/dev/null 2>&1 || true
                echo "Logger SIP PJSIP activé (voir: docker compose logs -f asterisk)"
                break
            fi
            sleep 1
        done
    ) &
fi

exec "$@"
