#!/usr/bin/env python3
"""Verify local firing/resolved Alertmanager delivery from the disabled worker."""

import http.server
import json
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import threading
import time
import urllib.request

from mission07_restore_drill import ROOT, run

PROMETHEUS = "prom/prometheus:v3.14.0@sha256:5ce7540c3c00ef4ab0c9d2c995c6a5b9c421f44b4a115d97a2c7af3b1c21cbb0"
ALERTMANAGER = "prom/alertmanager:v0.31.0@sha256:cc54cc450174ada901b32eb2538de5fc70ee259a1ac551ed38023f2ca2ad00e3"
COMPOSE = ["docker", "compose", "-f", "compose.yaml", "-f", "deploy/observability/compose.yaml"]


def main() -> None:
    metrics = urllib.request.urlopen("http://127.0.0.1:59090/metrics", timeout=3).read(1 << 20).decode()
    if re.search(r"^relaydesk_worker_outbound_enabled(?:\{[^}]*\})? 0(?:\.0+)?$", metrics, re.MULTILINE) is None:
        raise SystemExit("worker must be running with actual metrics and outbound disabled")
    project = json.loads(run(COMPOSE + ["config", "--format", "json"]))["name"]
    network = project + "_default"
    suffix = secrets.token_hex(4)
    alert_name, prometheus_name = "rd-m081-alert-" + suffix, "rd-m081-prom-" + suffix
    received: list[str] = []
    lock = threading.Lock()

    class Receiver(http.server.BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            try:
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(min(length, 1 << 20))
                payload = json.loads(body)
                with lock:
                    received.extend(item["status"] for item in payload.get("alerts", [])
                                    if item.get("labels", {}).get("alertname") == "RelayDeskOutboundDisabled")
                self.send_response(204)
                self.end_headers()
            except (ValueError, KeyError):
                self.send_error(400)

        def log_message(self, *_: object) -> None:
            return

    receiver = http.server.HTTPServer(("0.0.0.0", 0), Receiver)
    receiver_thread = threading.Thread(target=receiver.serve_forever, daemon=True)
    receiver_thread.start()
    started: list[str] = []
    try:
        scratch = ROOT / "tmp"
        scratch.mkdir(mode=0o700, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="relaydesk-m081-alert-", dir=scratch) as folder:
            private = Path(folder)
            private.chmod(0o700)
            alert_config = private / "alertmanager.yml"
            alert_config.write_text("route:\n  receiver: local\n  group_wait: 0s\n  group_interval: 5s\n"
                                    "  repeat_interval: 30s\nreceivers:\n- name: local\n  webhook_configs:\n"
                                    f"  - url: http://host.docker.internal:{receiver.server_port}/alerts\n"
                                    "    send_resolved: true\n")
            alert_config.chmod(0o600)
            prom_config = private / "prometheus.yml"
            prom_config.write_text("global:\n  scrape_interval: 5s\n  evaluation_interval: 5s\n"
                                   "rule_files:\n- /etc/prometheus/alerts.yml\nalerting:\n  alertmanagers:\n"
                                   f"  - static_configs:\n    - targets: ['{alert_name}:9093']\n"
                                   "scrape_configs:\n- job_name: relaydesk-worker\n  static_configs:\n"
                                   "  - targets: ['worker:9090']\n")
            prom_config.chmod(0o600)
            run(["docker", "run", "-d", "--pull=never", "--name", alert_name, "--network", network,
                 "-v", str(alert_config) + ":/etc/alertmanager/alertmanager.yml:ro", ALERTMANAGER,
                 "--config.file=/etc/alertmanager/alertmanager.yml", "--storage.path=/tmp/alertmanager"])
            started.append(alert_name)
            run(["docker", "run", "-d", "--pull=never", "--name", prometheus_name, "--network", network,
                 "-p", "127.0.0.1::9090", "-v", str(prom_config) + ":/etc/prometheus/prometheus.yml:ro",
                 "-v", str(ROOT / "deploy/observability/alerts.yml") + ":/etc/prometheus/alerts.yml:ro",
                 PROMETHEUS, "--config.file=/etc/prometheus/prometheus.yml"])
            started.append(prometheus_name)
            deadline = time.monotonic() + 190
            while time.monotonic() < deadline:
                with lock:
                    if "firing" in received:
                        break
                time.sleep(2)
            else:
                raise RuntimeError("actual disabled-worker alert was not delivered as firing")
            run(COMPOSE + ["stop", "worker"])
            deadline = time.monotonic() + 75
            while time.monotonic() < deadline:
                with lock:
                    if "resolved" in received:
                        print("PASS: actual disabled-worker metric delivered firing and resolved alerts")
                        return
                time.sleep(2)
            raise RuntimeError("disabled-worker alert did not resolve after worker stopped")
    finally:
        for name in reversed(started):
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, check=False)
        receiver.shutdown()
        receiver.server_close()
        receiver_thread.join(timeout=3)


if __name__ == "__main__":
    main()
