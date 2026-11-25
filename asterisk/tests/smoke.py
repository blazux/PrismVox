"""Run inside the network namespace of an isolated Asterisk test container."""
import base64
import hashlib
import json
import os
import socket
import struct
import time
import urllib.error
import urllib.parse
import urllib.request


AUTH = "Basic " + base64.b64encode(b"fixture:fixture-password").decode()
BASE = "http://127.0.0.1:8088/ari"


def request(method, path, params=None, authenticated=True):
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    headers = {"Authorization": AUTH} if authenticated else {}
    req = urllib.request.Request(url, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=5) as response:
            body = response.read()
            return response.status, json.loads(body) if body else None
    except urllib.error.HTTPError as error:
        raise AssertionError(f"{method} {path}: HTTP {error.code}: {error.read()!r}") from error


def websocket():
    connection = socket.create_connection(("127.0.0.1", 8088), timeout=5)
    key = base64.b64encode(os.urandom(16)).decode()
    connection.sendall((
        "GET /ari/events?app=fixture HTTP/1.1\r\n"
        "Host: 127.0.0.1:8088\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
        f"Authorization: {AUTH}\r\n\r\n"
    ).encode())
    response = b""
    while not response.endswith(b"\r\n\r\n"):
        response += connection.recv(1)
    assert response.startswith(b"HTTP/1.1 101 "), response
    accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest())
    assert accept.lower() in response.lower(), response
    return connection


def main():
    until = time.monotonic() + 30
    while True:
        try:
            _, info = request("GET", "/asterisk/info")
            break
        except (OSError, AssertionError):
            if time.monotonic() > until:
                raise
            time.sleep(0.2)
    assert info["system"]["version"] == "20.22.0", info["system"]
    try:
        request("GET", "/asterisk/info", authenticated=False)
        raise AssertionError("ARI accepted a request without credentials")
    except AssertionError as error:
        assert "HTTP 401" in str(error), error

    _, modules = request("GET", "/asterisk/modules")
    modules = {module["name"]: module for module in modules}
    for module in (
        "chan_pjsip.so", "chan_rtp.so", "app_stasis.so", "app_dial.so",
        "res_ari.so", "res_ari_channels.so", "res_ari_bridges.so", "res_ari_events.so",
        "res_http_websocket.so", "res_pjsip.so", "res_pjsip_refer.so", "res_srtp.so",
        "res_rtp_asterisk.so", "res_stasis_snoop.so", "res_stasis_playback.so",
        "codec_alaw.so", "codec_ulaw.so", "codec_g722.so", "codec_resample.so",
        "codec_amr.so", "res_format_attr_amr.so", "format_ogg_opus_open_source.so",
    ):
        assert module in modules, f"required module missing: {module}"
        assert modules[module]["status"] == "Running", f"required module not running: {modules[module]}"
    events = websocket()  # Basic header only; no credentials in the URL.
    channels, bridges = [], []
    receiver = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    receiver.bind(("127.0.0.1", 0))
    receiver.settimeout(5)
    try:
        _, bridge = request("POST", "/bridges", {"type": "mixing", "bridgeId": "fixture-a"})
        bridges.append(bridge["id"])
        _, other = request("POST", "/bridges", {"type": "mixing", "bridgeId": "fixture-b"})
        bridges.append(other["id"])
        _, media = request("POST", "/channels/externalMedia", {
            "app": "fixture", "channelId": "fixture-media", "format": "slin16",
            "external_host": f"127.0.0.1:{receiver.getsockname()[1]}",
        })
        channels.append(media["id"])
        request("POST", f"/bridges/{bridge['id']}/addChannel", {"channel": media["id"]})
        # Playback is generated locally; no STT, model, TTS or phone is contacted.
        request("POST", f"/bridges/{bridge['id']}/play", {"media": "tone:440/500"})
        packet, address = receiver.recvfrom(4096)
        assert packet[0] >> 6 == 2 and len(packet) > 12, "no valid RTP audio received"
        assert packet[1] & 127 == 118, f"unexpected slin16 payload type: {packet[1] & 127}"
        # Send synthetic 16 kHz PCM back through ExternalMedia, using its RTP port.
        receiver.sendto(struct.pack("!BBHII", 0x80, 118, 1, 320, 42) + bytes(640), address)
        _, snoop = request("POST", f"/channels/{media['id']}/snoop", {
            "app": "fixture", "snoopId": "fixture-snoop", "spy": "in", "whisper": "none",
        })
        channels.append(snoop["id"])
        request("POST", f"/bridges/{other['id']}/addChannel", {"channel": snoop["id"]})
        # Move a live leg between bridges, as in an attended-transfer handoff.
        request("POST", f"/bridges/{other['id']}/addChannel", {"channel": media["id"]})
        _, moved = request("GET", f"/bridges/{other['id']}")
        assert set(moved["channels"]) == set(channels), moved
        request("POST", f"/channels/{media['id']}/moh")
        request("DELETE", f"/channels/{media['id']}/moh")
    finally:
        for channel in reversed(channels):
            request("DELETE", "/channels/" + channel)
        for bridge in bridges:
            request("DELETE", "/bridges/" + bridge)
        receiver.close()
        events.close()
    assert request("GET", "/channels")[1] == []
    assert request("GET", "/bridges")[1] == []
    print("Asterisk 20.22: authenticated ARI HTTP/WebSocket, modules, ExternalMedia RTP, playback, snoop, bridge handoff, hold and cleanup passed")


if __name__ == "__main__":
    main()
