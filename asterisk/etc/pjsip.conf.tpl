; ── Transports ─────────────────────────────────────────────────────────────────
[transport-udp]
type = transport
protocol = udp
bind = 0.0.0.0:5060

[transport-tls]
type = transport
protocol = tls
bind = 0.0.0.0:5061
method = tlsv1_2
verify_server = ${SIP_TLS_VERIFY_SERVER}
ca_list_file = ${SIP_TLS_CA_FILE}
verify_client = no

; ── SIP trunk ──────────────────────────────────────────────────────────────────
; auth / aor / endpoint / identify / registration live in a shared volume so the
; UI can re-render them and reload PJSIP without rebuilding the container.
; Seeded from the SIP_* env at first boot (see docker-entrypoint.sh).
#include "dynamic/pjsip_trunk.conf"
