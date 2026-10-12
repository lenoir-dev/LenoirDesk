#!/usr/bin/env python3
"""Verify an executor-generated business trace in an isolated local Tempo."""

import os
import secrets
import subprocess
import time
import urllib.error
import urllib.request

from mission07_restore_drill import ROOT, run

TEMPO = "grafana/tempo:3.1.0@sha256:3076b8dcdfb32fd6bc5ccef85e7b7313e6199b9cb84366257fc17ecb696db5fd"


def port(container: str, number: int) -> str:
    return run(["docker", "port", container, str(number) + "/tcp"]).rsplit(":", 1)[-1]


def main() -> None:
    running = run(["docker", "compose", "ps", "--status", "running", "--services"])
    if "worker" in running.splitlines():
        raise SystemExit("stop the root Compose worker before the isolated trace gate")
    name = "rd-m081-tempo-" + secrets.token_hex(5)
    try:
        run(["docker", "run", "-d", "--pull=never", "--name", name,
             "-p", "127.0.0.1::3200", "-p", "127.0.0.1::4318",
             "-v", str(ROOT / "deploy/observability/tempo.yaml") + ":/etc/tempo.yaml:ro",
             TEMPO, "-config.file=/etc/tempo.yaml"])
        query = "http://127.0.0.1:" + port(name, 3200)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                with urllib.request.urlopen(query + "/ready", timeout=2) as response:
                    if response.status == 200:
                        break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.5)
        else:
            raise RuntimeError("isolated Tempo did not become ready")
        env = dict(os.environ)
        env.update({"RELAYDESK_M081_TRACE": "1", "RELAYDESK_M081_TEMPO_QUERY_URL": query,
                    "RELAYDESK_OTLP_TRACES_URL": "http://127.0.0.1:" + port(name, 4318) + "/v1/traces"})
        test = subprocess.run(["go", "test", "-tags", "integration", "-count=1", "-timeout=90s",
                               "-run", "^TestMission081BusinessActionTraceInTempo$", "-v", "./tests/integration"],
                              cwd=ROOT, env=env, check=False)
        if test.returncode:
            raise SystemExit(test.returncode)
    finally:
        subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, check=False)


if __name__ == "__main__":
    main()
