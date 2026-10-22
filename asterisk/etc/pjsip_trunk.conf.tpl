; ── SIP trunk ──────────────────────────────────────────────────────────────────
; Rendered from the SIP_* environment at first boot, then managed from the UI
; (Configuration → SIP). Both renderers use the same ${SIP_*} placeholders:
;   - asterisk/docker-entrypoint.sh via envsubst (bootstrap)
;   - ui/app/main.py via string.Template (on "Apply")
; Do not edit this rendered file by hand — UI changes overwrite it.
;
; SIP_TRANSPORT, SIP_MEDIA_ENCRYPTION_LINE and SIP_PORT are not raw env vars —
; both renderers derive them from sip_transport_mode ("tls", default: SRTP over
; TLS on SIP_TLS_PORT — required by BroadWorks) or "udp" (plain SIP/RTP, no
; encryption, on SIP_UDP_PORT), so this template stays mode-agnostic.

; ── Auth ──────────────────────────────────────────────────────────────────────
[phoneagent-auth]
type = auth
auth_type = userpass
username = ${SIP_USERNAME}
password = ${SIP_PASSWORD}

; ── AOR ───────────────────────────────────────────────────────────────────────
[phoneagent-aor]
type = aor
; Logical domain → Request-URI and To will use the SIP domain
contact = sip:${SIP_DOMAIN}

; ── Endpoint ──────────────────────────────────────────────────────────────────
[phoneagent-endpoint]
type = endpoint
transport = ${SIP_TRANSPORT}
context = from-trunk
allow = !all,g722,alaw,ulaw
outbound_auth = phoneagent-auth
aors = phoneagent-aor
from_user = ${SIP_USERNAME}
from_domain = ${SIP_DOMAIN}
; The outbound From caller ID is set by the agent on the originated channel (ARI
; callerId) from sip_callerid_name — an endpoint `callerid` doesn't win for
; ARI-originated channels.
; Force media through Asterisk so the agent receives RTP (not SRTP directly)
direct_media = no
; SRTP via SDES in tls mode (RTP/SAVP, required by BroadWorks over TLS) — no
; optimistic, so outbound INVITEs are always SAVP. Blank (udp mode): plain RTP/AVP.
${SIP_MEDIA_ENCRYPTION_LINE}
; RTP keepalive — required for NAT traversal: SBC learns Asterisk's real IP:port
rtp_keepalive = 5

; ── Identify ──────────────────────────────────────────────────────────────────
[phoneagent-identify]
type = identify
endpoint = phoneagent-endpoint
match = ${SIP_REGISTRAR_MATCH}

; ── Registration ──────────────────────────────────────────────────────────────
[phoneagent-registration]
type = registration
transport = ${SIP_TRANSPORT}
outbound_auth = phoneagent-auth
server_uri = sip:${SIP_REGISTRAR}:${SIP_PORT}
client_uri = sip:${SIP_USERNAME}@${SIP_DOMAIN}
contact_user = ${SIP_USERNAME}
expiration = 1800
retry_interval = 30
forbidden_retry_interval = 300
fatal_retry_interval = 30
