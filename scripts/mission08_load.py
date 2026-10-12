#!/usr/bin/env python3
"""Run the 20-tenant 100/s profile on disposable PostgreSQL/NATS/Redis."""

import base64
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import subprocess
import time

from mission07_restore_drill import ROOT, ready, run

POSTGRES = "postgres:18.6-alpine3.23@sha256:885cf05d376c7cf27afef02073e6bdac3841252537f16e244fd1c1e6a7c99fb1"
NATS = "nats:2.12.4-alpine3.22@sha256:31c6ed3b2da61645aaa3ad9217b5a52b34b6ebd555ecb71259cd7723c59ae1ea"
REDIS = "redis:8.2.10-alpine3.22@sha256:b51665e66f00759be7c3152ad5ac3c66fb2f619c13ef62dea7cc1f9914524635"


def port(container: str, number: int) -> str:
    return run(["docker", "port", container, str(number) + "/tcp"]).rsplit(":", 1)[-1]


def main() -> None:
    suffix = secrets.token_hex(5)
    network = "relaydesk-load-" + suffix
    pg, nats, redis = ("rd-load-pg-" + suffix, "rd-load-nats-" + suffix, "rd-load-redis-" + suffix)
    private = ROOT / "tmp" / ("mission08-load-" + suffix)
    private.mkdir(mode=0o700)
    os.chmod(private, 0o700)
    password = secrets.token_hex(24)  # synthetic isolated services only
    started = []
    run(["docker", "network", "create", network])
    try:
        run(["docker", "run", "-d", "--pull=never", "--name", pg, "--network", network,
             "-p", "127.0.0.1::5432", "-e", "POSTGRES_USER=postgres",
             "-e", "POSTGRES_PASSWORD=" + password, "-e", "POSTGRES_DB=postgres",
             "-e", "RELAYDESK_DB_NAME=relaydesk", "-e", "RELAYDESK_MIGRATE_PASSWORD=" + password,
             "-e", "RELAYDESK_RUNTIME_PASSWORD=" + password,
             "-e", "RELAYDESK_APP_PASSWORD=" + password, "-e", "RELAYDESK_AUTH_PASSWORD=" + password,
             "-v", str(ROOT / "deploy/postgres/01-roles.sh") + ":/docker-entrypoint-initdb.d/01-roles.sh:ro",
             POSTGRES])
        started.append(pg)
        ready(pg)
        run(["docker", "run", "-d", "--pull=never", "--name", nats, "--network", network,
             "-p", "127.0.0.1::4222", NATS, "--jetstream", "--store_dir", "/tmp/nats"])
        started.append(nats)
        run(["docker", "run", "-d", "--pull=never", "--name", redis, "--network", network,
             "-p", "127.0.0.1::6379", REDIS, "redis-server", "--appendonly", "yes"])
        started.append(redis)
        migrate_env = dict(os.environ)
        migrate_env["GOOSE_DBSTRING"] = ("postgres://relaydesk_migrate:" + password +
                                         "@" + pg + ":5432/relaydesk?sslmode=disable")
        run(["docker", "run", "--rm", "--pull=never", "--network", network,
             "-e", "GOOSE_DBSTRING", "-e", "GOOSE_DRIVER=postgres",
             "-e", "GOOSE_MIGRATION_DIR=/app/migrations", "--entrypoint", "/app/goose",
             "relaydesk-migrate:latest", "up"], env=migrate_env)
        pg_port, nats_port, redis_port = port(pg, 5432), port(nats, 4222), port(redis, 6379)
        envfile = private / "synthetic.env"
        lines = []
        for role, key in (("relaydesk_runtime", "RELAYDESK_DATABASE_URL"),
                          ("relaydesk_app", "RELAYDESK_APP_DATABASE_URL"),
                          ("relaydesk_auth", "RELAYDESK_AUTH_DATABASE_URL"),
                          ("relaydesk_migrate", "RELAYDESK_MIGRATION_DATABASE_URL")):
            lines.append(key + "=postgres://" + role + ":" + password + "@127.0.0.1:" + pg_port +
                         "/relaydesk?sslmode=disable")
        lines.append("RELAYDESK_ENCRYPTION_KEY_B64=" + base64.b64encode(secrets.token_bytes(32)).decode())
        with os.fdopen(os.open(envfile, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "w") as out:
            out.write("\n".join(lines) + "\n")
        test_env = dict(os.environ)
        test_env.update({"RELAYDESK_TEST_ENV_FILE": str(envfile),
                         "RELAYDESK_TEST_NATS_URL": "nats://127.0.0.1:" + nats_port,
                         "RELAYDESK_M08_REDIS_URL": "redis://127.0.0.1:" + redis_port,
                         "RELAYDESK_M08_LOAD": "1"})
        result = subprocess.run(["go", "test", "-tags", "integration", "-count=1", "-timeout=6m",
                                 "-run", "^TestMission08LoadProfile$", "-v", "./tests/integration"],
                                cwd=ROOT, env=test_env, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, check=False)
        output = result.stdout.decode(errors="replace")
        match = re.search(r"MISSION08_LOAD_JSON (\{[^\n]+\})", output)
        if match:
            print(match.group(1))
        stats = run(["docker", "stats", "--no-stream", "--format", "{{.Name}} {{.CPUPerc}} {{.MemUsage}}",
                     pg, nats, redis])
        print("isolated container stats: " + stats.replace("\n", "; "))
        resources = run(["docker", "info", "--format", "{{json .NCPU}} {{json .MemTotal}} {{json .ServerVersion}}"])
        print("benchmark environment: " + json.dumps({"host": platform.platform(),
              "host_machine": platform.machine(), "docker_cpu_memory_version": resources,
              "dataset": "20 tenants; 6000 signed invoice.paid events; shared synthetic invoice per tenant; seed=808",
              "source_commit": run(["git", "rev-parse", "HEAD"]),
              "dirty_source": bool(run(["git", "status", "--porcelain"]))}, sort_keys=True))
        print("load test exit=" + str(result.returncode))
        if result.returncode:
            safe = re.findall(r"(?:FAIL|panic|mission08_load_test\.go:\d+)[^\n]*", output)
            raise RuntimeError("isolated load failed: " + "; ".join(safe[-5:])[:400])
    finally:
        for name in reversed(started):
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, check=False)
        subprocess.run(["docker", "network", "rm", network], stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, check=False)
        shutil.rmtree(private)


if __name__ == "__main__":
    main()
