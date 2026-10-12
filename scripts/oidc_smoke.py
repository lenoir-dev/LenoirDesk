#!/usr/bin/env python3
"""Exercise the real local Dex browser login without printing credentials."""

import html
import http.cookiejar
import json
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request


ORIGIN = "http://localhost:58080"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


def env_value(name: str) -> str:
    for line in Path(".env").read_text().splitlines():
        if line.startswith(name + "="):
            return line.split("=", 1)[1].strip("'")
    raise RuntimeError(f"missing local configuration: {name}")


def main() -> None:
    jar = http.cookiejar.CookieJar()
    client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    for _ in range(30):
        try:
            with client.open(ORIGIN + "/health/ready", timeout=2) as response:
                if response.status == 204:
                    break
        except urllib.error.URLError:
            pass
        time.sleep(1)
    else:
        raise RuntimeError("RelayDesk API did not become ready")
    try:
        urllib.request.build_opener(NoRedirect()).open(ORIGIN + "/auth/login", timeout=15)
    except urllib.error.HTTPError as exc:
        if exc.code != 302:
            raise RuntimeError("OIDC authorization did not redirect") from None
        authorization = urllib.parse.urlsplit(exc.headers.get("Location", ""))
        if (authorization.scheme != "http" or authorization.netloc != "127.0.0.1:5556" or
                urllib.parse.parse_qs(authorization.query).get("redirect_uri") != [ORIGIN + "/auth/callback"]):
            raise RuntimeError("OIDC authorization redirect URI is inconsistent") from None
    else:
        raise RuntimeError("OIDC authorization did not redirect")
    login_page = client.open(ORIGIN + "/auth/login", timeout=15).read().decode()
    match = re.search(r'<form[^>]*action="([^"]+)"', login_page)
    if not match:
        raise RuntimeError("Dex login form unavailable")
    action = urllib.parse.urljoin("http://127.0.0.1:5556", html.unescape(match.group(1)))
    data = urllib.parse.urlencode({
        "login": "developer@relaydesk.invalid",
        "password": env_value("RELAYDESK_DEV_OIDC_PASSWORD"),
    }).encode()
    request = urllib.request.Request(action, data=data, method="POST")
    page = client.open(request, timeout=15).read().decode()
    if "Signed in as developer@relaydesk.invalid" not in page:
        raise RuntimeError("OIDC login did not produce a RelayDesk session")
    session_cookies = [cookie for cookie in jar if cookie.name == "relaydesk_dev_session"]
    if (len(session_cookies) != 1 or session_cookies[0].domain_specified or
            session_cookies[0].path != "/" or session_cookies[0].secure):
        raise RuntimeError("local session cookie lost host-only scope")
    me = json.load(client.open(ORIGIN + "/v1/me", timeout=10))
    csrf = me["csrf_token"]
    invalid = urllib.request.Request(
        ORIGIN + "/v1/tenants",
        data=json.dumps({"name": "must not be created"}).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        client.open(invalid, timeout=10)
    except urllib.error.HTTPError as exc:
        if exc.code != 403 or json.load(exc).get("error", {}).get("code") != "csrf_invalid":
            raise RuntimeError("missing CSRF token was not rejected") from None
    else:
        raise RuntimeError("missing CSRF token was accepted")
    request = urllib.request.Request(
        ORIGIN + "/v1/tenants",
        data=json.dumps({"name": "OIDC smoke workspace"}).encode(),
        headers={"Content-Type": "application/json", "X-CSRF-Token": csrf},
        method="POST",
    )
    with client.open(request, timeout=10) as response:
        if response.status != 201:
            raise RuntimeError("workspace creation failed")
        workspace = json.load(response)
    with client.open(ORIGIN + "/v1/tenants/" + workspace["id"], timeout=10) as response:
        if response.status != 200:
            raise RuntimeError("workspace read failed")
    overview = client.open(ORIGIN + "/ui/tenants/" + workspace["id"], timeout=10).read().decode()
    if ("OIDC smoke workspace" not in overview or "<strong>Owner</strong>" not in overview or
            f'href="/ui/tenants/{workspace["id"]}/connections"' not in overview or
            "Getting started" not in overview or
            "No Stripe events have been received" not in overview or "No runs yet" not in overview):
        raise RuntimeError("workspace overview did not reflect database state")
    connections = client.open(ORIGIN + "/ui/tenants/" + workspace["id"] + "/connections", timeout=10).read().decode()
    if "No providers connected" not in connections or "Set up Stripe" not in connections:
        raise RuntimeError("connection onboarding did not reflect empty workspace")
    logout = urllib.request.Request(
        ORIGIN + "/auth/logout",
        headers={"X-CSRF-Token": csrf},
        method="POST",
    )
    with client.open(logout, timeout=10) as response:
        if response.status != 204:
            raise RuntimeError("logout failed")
    try:
        client.open(ORIGIN + "/v1/me", timeout=10)
    except urllib.error.HTTPError as exc:
        if exc.code != 401:
            raise
    else:
        raise RuntimeError("revoked session still works")
    print("PASS: Dex localhost redirect, login, host-only session, CSRF denial, owner workspace, logout")


if __name__ == "__main__":
    main()
