"""Browser sessions; HTTP Basic and agent Bearer access remain in main.py."""
import hashlib
import hmac
import html
import secrets
import time
from collections import OrderedDict

from fastapi import Form, Request
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse

COOKIE = "prismvox_session"
SESSION_SECONDS = 8 * 60 * 60
PUBLIC_PATHS = frozenset(("/login", "/auth/login", "/static/login.css",
                          "/static/login.js", "/static/app.css", "/static/logo.svg"))
PAGE_HEADERS = {
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "same-origin",
    "Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
}


def _signature(value, user, password):
    key = hashlib.sha256(("prismvox-browser-v1\0" + user + "\0" + password).encode()).digest()
    return hmac.new(key, value.encode(), hashlib.sha256).hexdigest()


def new_session(user, password):
    value = f"{int(time.time()) + SESSION_SECONDS}.{secrets.token_hex(24)}"
    return value + "." + _signature(value, user, password)


def session_ok(request, user, password):
    if not password:
        return False
    token = request.cookies.get(COOKIE, "")
    if len(token) > 160:
        return False
    try:
        expiry, nonce, signature = token.split(".")
        now = int(time.time())
        if not now < int(expiry) <= now + SESSION_SECONDS or len(nonce) != 48:
            return False
        return hmac.compare_digest(signature, _signature(expiry + "." + nonce, user, password))
    except (ValueError, TypeError):
        return False


def same_origin(request):
    # Browsers send Origin on form/fetch POSTs. An absent Origin is accepted for
    # non-browser login clients; cookie-authenticated writes require it below.
    origin = request.headers.get("origin")
    if request.headers.get("sec-fetch-site") == "cross-site":
        return False
    # Compare the browser origin to Host, also behind a TLS-terminating proxy.
    # Do not trust arbitrary forwarded headers to decide who may write.
    host = request.headers.get("host", "")
    return origin is None or origin in ("http://" + host, "https://" + host)


def secure_cookie(request):
    # A same-host HTTPS Origin also covers proxies terminating TLS upstream.
    return request.url.scheme == "https" or request.headers.get("origin") == "https://" + request.headers.get("host", "")


def cookie_write_ok(request):
    return request.method in ("GET", "HEAD", "OPTIONS") or (
        bool(request.headers.get("origin")) and same_origin(request))


def install(app, static_dir, credentials):
    # Bound memory and login attempts without storing credentials or requiring a
    # session database. Both HTTP/HTTPS workers verify the same signed cookies.
    failures = OrderedDict()

    def page(error="", username="", status=200):
        content = (static_dir / "login.html").read_text()
        content = content.replace("{{ERROR}}", html.escape(error, quote=True))
        content = content.replace("{{USERNAME}}", html.escape(username, quote=True))
        fallback = {"credentials": "Incorrect username or password.",
                    "rate_limit": "Too many attempts. Please try again in a minute."}.get(error, "")
        content = content.replace("{{ERROR_MESSAGE}}", fallback)
        return HTMLResponse(content, status_code=status, headers=PAGE_HEADERS)

    @app.get("/login", response_class=HTMLResponse)
    async def login_page(request: Request):
        user, password = credentials()
        if not password or session_ok(request, user, password):
            return RedirectResponse("/", status_code=303, headers={"Cache-Control": "no-store"})
        return page()

    @app.post("/auth/login")
    async def login(request: Request, username: str = Form(""), password: str = Form("")):
        if not same_origin(request):
            return JSONResponse({"detail": "Forbidden"}, status_code=403, headers=PAGE_HEADERS)
        user, expected = credentials()
        wants_json = "application/json" in request.headers.get("accept", "")
        address = request.client.host if request.client else "unknown"
        now = time.monotonic()
        attempts, start = failures.get(address, (0, now))
        if now - start >= 60:
            attempts, start = 0, now
        if attempts >= 10:
            headers = {**PAGE_HEADERS, "Retry-After": str(max(1, int(60 - (now - start))))}
            return (JSONResponse({"error": "rate_limit"}, status_code=429, headers=headers)
                    if wants_json else page("rate_limit", username[:256], 429))
        valid = (bool(expected) and len(username) <= 256 and len(password) <= 4096
                 and secrets.compare_digest(username.encode(), user.encode())
                 and secrets.compare_digest(password.encode(), expected.encode()))
        if not valid:
            failures[address] = (attempts + 1, start)
            failures.move_to_end(address)
            if len(failures) > 1024:
                failures.popitem(last=False)
            return (JSONResponse({"error": "credentials"}, status_code=401, headers=PAGE_HEADERS)
                    if wants_json else page("credentials", username[:256], 401))
        failures.pop(address, None)
        response = (JSONResponse({"ok": True}, headers=PAGE_HEADERS) if wants_json else
                    RedirectResponse("/", status_code=303, headers={"Cache-Control": "no-store"}))
        response.set_cookie(COOKIE, new_session(user, expected), max_age=SESSION_SECONDS,
                            httponly=True, secure=secure_cookie(request), samesite="strict", path="/")
        return response

    @app.post("/auth/logout")
    async def logout(request: Request):
        if not same_origin(request):
            return JSONResponse({"detail": "Forbidden"}, status_code=403)
        response = RedirectResponse("/login", status_code=303, headers={"Cache-Control": "no-store"})
        response.delete_cookie(COOKIE, path="/", httponly=True, samesite="strict",
                               secure=secure_cookie(request))
        return response
