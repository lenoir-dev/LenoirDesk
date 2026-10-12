#!/usr/bin/env python3
"""Run only the dedicated, synthetic Mission 06 Compose project."""

import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
STATE = ROOT / "tmp" / "lenoirdesk-demo"
CONFIG = STATE / "synthetic.env"
MARKER = STATE / "identity.json"
COMPOSE = ROOT / "deploy" / "demo" / "compose.yaml"
PROJECT = "lenoirdesk_m06_demo"


def safe_env():
    # Never pass an owner's provider or Stage 0 credentials to demo processes.
    allowed = ("PATH", "HOME", "TMPDIR", "USER", "LANG", "DOCKER_CONFIG", "GOCACHE",
               "GOMODCACHE", "GOPATH", "GOPROXY", "GOSUMDB", "GOTOOLCHAIN")
    return {name: os.environ[name] for name in allowed if name in os.environ}


def compose(*args):
    return subprocess.run(["docker", "compose", "-p", PROJECT, "-f", str(COMPOSE),
                           "--env-file", str(CONFIG), *args], cwd=ROOT, env=safe_env(), check=True)


def identity_check():
    if STATE.is_symlink() or CONFIG.is_symlink() or MARKER.is_symlink():
        raise SystemExit("demo state must not contain symlinks")
    if not MARKER.is_file() or not CONFIG.is_file():
        raise SystemExit("dedicated demo marker/config missing; no containers or volumes were changed")
    identity = json.loads(MARKER.read_text())
    if identity != {"root": str(ROOT), "project": PROJECT, "schema": 1}:
        raise SystemExit("demo identity mismatch; no containers or volumes were changed")
    if CONFIG.stat().st_mode & 0o077:
        raise SystemExit("demo config permissions must be 0600")


def create_config():
    if STATE.exists():
        raise SystemExit("demo state exists; use make demo-reset or make demo-down first")
    STATE.mkdir(mode=0o700, parents=True)
    os.chmod(STATE, 0o700)
    passwords = {role: secrets.token_hex(24) for role in ("POSTGRES", "MIGRATE", "RUNTIME", "APP", "AUTH")}
    lines = [f"RELAYDESK_{role}_PASSWORD={value}" for role, value in passwords.items()]
    for role, key in (("migrate", "MIGRATION_DATABASE_URL"), ("runtime", "DATABASE_URL"),
                      ("app", "APP_DATABASE_URL"), ("auth", "AUTH_DATABASE_URL")):
        lines.append(f"RELAYDESK_{key}=postgres://relaydesk_{role}:{passwords[role.upper()]}"
                     "@postgres:5432/relaydesk?sslmode=disable")
    lines.append("RELAYDESK_ENCRYPTION_KEY_B64=" +
                 base64.b64encode(secrets.token_bytes(32)).decode("ascii"))
    for path, content in ((CONFIG, "\n".join(lines) + "\n"),
                          (MARKER, json.dumps({"root": str(ROOT), "project": PROJECT, "schema": 1}) + "\n")):
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
            out.write(content)


def remove_state():
    for path in (CONFIG, MARKER):
        path.unlink()
    STATE.rmdir()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("start", "check", "down", "reset"))
    action = parser.parse_args().action
    if action in ("start", "check"):
        create_config()
        try:
            compose("config", "--quiet")
            compose("up", "-d", "--wait", "postgres", "nats", "redis")
            compose("build", "migrate")
            compose("run", "--rm", "migrate")
            env = safe_env()
            env.update({"RELAYDESK_TEST_ENV_FILE": str(CONFIG),
                        "RELAYDESK_TEST_PG_PORT": "59432",
                        "RELAYDESK_TEST_NATS_URL": "nats://127.0.0.1:59422",
                        "RELAYDESK_INTERACTIVE_DEMO": "1"})
            if action == "check":
                env["RELAYDESK_DEMO_AUTORUN"] = "1"
            print("\nSynthetic-only demo ready. Local sign-in uses an in-process test OIDC identity.", flush=True)
            print("Keep this command running; use make demo-reset to start clean next time.", flush=True)
            subprocess.run(["go", "test", "-tags", "integration", "-timeout=0", "-count=1",
                            "-run", "^TestMission06InteractiveDemo$", "-v", "./tests/integration"],
                           cwd=ROOT, env=env, check=True)
            if action == "check":
                compose("down", "--volumes", "--remove-orphans")
                remove_state()
                print("Synthetic demo contract PASS; dedicated containers and volumes removed.")
        except KeyboardInterrupt:
            print("Demo HTTP server stopped. Isolated containers remain; run make demo-down.")
        return
    identity_check()
    compose("down", *(["--volumes", "--remove-orphans"] if action == "reset" else []))
    if action == "reset":
        remove_state()
        print("Only the dedicated synthetic demo project was reset. Run make demo-local to start fresh.")
    else:
        print("Dedicated demo containers stopped. Synthetic volumes retained; run make demo-reset to erase them.")


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as exc:
        raise SystemExit(f"demo {exc.cmd[0]} step failed with status {exc.returncode}; check Docker and make demo-reset") from None
