#!/usr/bin/env python3
"""Isolated, synthetic PostgreSQL backup/restore and uncertain-effect drill.

Never points at the developer database. The dump contains only generated data.
No restored worker is started before the safety gate and operator review.
"""

from __future__ import annotations

import base64
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
POSTGRES = "postgres:18.6-alpine3.23@sha256:885cf05d376c7cf27afef02073e6bdac3841252537f16e244fd1c1e6a7c99fb1"
NATS = "nats:2.12.4-alpine3.22@sha256:31c6ed3b2da61645aaa3ad9217b5a52b34b6ebd555ecb71259cd7723c59ae1ea"
MIGRATE = "relaydesk-migrate:latest"


def run(args: list[str], *, stdin: str | None = None, env: dict[str, str] | None = None,
        output=None) -> str:
    result = subprocess.run(args, input=stdin.encode() if stdin is not None else None,
                            stdout=output if output is not None else subprocess.PIPE,
                            stderr=subprocess.PIPE, env=env, check=False)
    if result.returncode:
        if "pg_dump" in args:
            raise RuntimeError("synthetic pg_dump failed: " + result.stderr.decode(errors="replace")[:500])
        if args[:2] == ["go", "test"]:
            diagnostic = result.stdout.decode(errors="replace") + result.stderr.decode(errors="replace")
            match = re.search(r"mission07_restore_gate_test\.go:(\d+)", diagnostic)
            status = re.search(r"initial signed event status=(\d+)", diagnostic)
            raise RuntimeError("restored CRM integration test failed at assertion line " +
                               (match.group(1) if match else "unknown") +
                               ("; HTTP status " + status.group(1) if status else "") + "; output withheld")
        raise RuntimeError("isolated restore command failed; arguments and output withheld")
    return "" if output is not None else result.stdout.decode().strip()


def sql(container: str, statement: str) -> str:
    return run(["docker", "exec", "-i", container, "psql", "-X", "-v", "ON_ERROR_STOP=1",
                "-At", "-U", "relaydesk_migrate", "-d", "relaydesk"], stdin=statement)


def ready(container: str) -> None:
    for _ in range(60):
        result = subprocess.run(["docker", "exec", container, "pg_isready", "-U", "postgres",
                                 "-d", "relaydesk"], stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, check=False)
        if result.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError("isolated PostgreSQL did not become ready")


def main() -> None:
    suffix = secrets.token_hex(5)
    network, source, target, broker = ("relaydesk-m07-" + suffix, "rd-m07-src-" + suffix,
                                       "rd-m07-dst-" + suffix, "rd-m07-nats-" + suffix)
    dump_dir = ROOT / "tmp" / ("mission07-restore-" + suffix)
    dump_dir.mkdir(parents=True, mode=0o700)
    os.chmod(dump_dir, 0o700)
    dump_path = dump_dir / "synthetic.dump"
    external_marker = dump_dir / "synthetic-external-effect.json"
    password = secrets.token_hex(24)
    run(["docker", "network", "create", network])
    started: list[str] = []
    try:
        for container in (source, target):
            publish = ["-p", "127.0.0.1::5432"] if container == target else []
            run(["docker", "run", "-d", "--name", container, "--network", network, *publish,
                 "-e", "POSTGRES_USER=postgres", "-e", "POSTGRES_PASSWORD=" + password,
                 "-e", "POSTGRES_DB=postgres", "-e", "RELAYDESK_DB_NAME=relaydesk",
                 "-e", "RELAYDESK_MIGRATE_PASSWORD=" + password,
                 "-e", "RELAYDESK_RUNTIME_PASSWORD=" + password,
                 "-e", "RELAYDESK_APP_PASSWORD=" + password,
                 "-e", "RELAYDESK_AUTH_PASSWORD=" + password,
                 "-v", str(ROOT / "deploy/postgres/01-roles.sh") + ":/docker-entrypoint-initdb.d/01-roles.sh:ro",
                 POSTGRES])
            started.append(container)
            ready(container)

        migration_env = dict(os.environ)
        migration_env["GOOSE_DBSTRING"] = "postgres://relaydesk_migrate:" + password + "@" + source + ":5432/relaydesk?sslmode=disable"
        run(["docker", "run", "--rm", "--network", network, "-e", "GOOSE_DBSTRING",
             "-e", "GOOSE_DRIVER=postgres", "-e", "GOOSE_MIGRATION_DIR=/app/migrations",
             "--entrypoint", "/app/goose", MIGRATE, "up"], env=migration_env)
        source_migration = int(sql(source, "SELECT max(version_id) FROM goose_db_version WHERE is_applied;"))
        sql(source, """
WITH u AS (INSERT INTO users(oidc_issuer,oidc_subject,email)
 VALUES('urn:relaydesk:synthetic','restore-owner','owner@example.invalid') RETURNING id),
t AS (INSERT INTO tenants(name) VALUES('Synthetic restore drill') RETURNING id),
m AS (INSERT INTO memberships(tenant_id,user_id,role)
 SELECT t.id,u.id,'owner' FROM t,u RETURNING id),
s AS (INSERT INTO user_sessions(user_id,token_hash,expires_at)
 SELECT u.id,decode(repeat('ab',32),'hex'),now()+interval '1 day' FROM u RETURNING id),
c AS (INSERT INTO connections(tenant_id,provider,provider_account_id,mode,status,
 webhook_ref_hash,health_status,execution_environment)
 SELECT t.id,'stripe','acct_synthetic_restore','test','active',decode(repeat('01',32),'hex'),
 'verified','simulation' FROM t RETURNING tenant_id,id),
r AS (INSERT INTO automation_revisions(tenant_id,connection_id,revision,status,
 template_key,config) SELECT c.tenant_id,c.id,1,'active','subscription_billing_sync.v1',
 '{"slack_channel_id":"C_SYNTHETIC"}'::jsonb FROM c RETURNING tenant_id,id,connection_id),
a AS (INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,
 stripe_connection_id,customer_id,invoice_id)
 SELECT r.tenant_id,r.id,'slack_invoice_paid','restore|synthetic|invoice',
 r.connection_id,'cus_synthetic','in_synthetic' FROM r RETURNING id)
SELECT (SELECT count(*) FROM m),(SELECT count(*) FROM s),(SELECT count(*) FROM a);
""")
        if sql(source, "SELECT count(*) FROM business_actions WHERE kind='slack_invoice_paid' AND status='pending';") != "1":
            raise RuntimeError("synthetic durable work was not created")

        backup_start = time.monotonic()
        with os.fdopen(os.open(dump_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "wb") as dump:
            run(["docker", "exec", source, "pg_dump", "-U", "postgres", "-Fc",
                 "--no-owner", "-d", "relaydesk"], output=dump)
        backup_seconds = round(time.monotonic() - backup_start, 3)
        dump_bytes = dump_path.stat().st_size
        boundary = time.time()

        # A synthetic external witness lives outside PostgreSQL and its dump.
        # This represents a Slack acceptance that the older backup cannot know.
        with os.fdopen(os.open(external_marker, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "w") as witness:
            json.dump({"channel": "C_SYNTHETIC", "ts": "1234567890.123456",
                       "accepted_at": time.time()}, witness)
        sql(source, "UPDATE business_actions SET status='succeeded',slack_channel='C_SYNTHETIC',"
                    "slack_ts='1234567890.123456' WHERE kind='slack_invoice_paid';")
        failure_at = time.time()

        restore_start = time.monotonic()
        with dump_path.open("rb") as dump:
            restored = subprocess.run(["docker", "exec", "-i", target, "pg_restore", "-U",
                                       "relaydesk_migrate", "--clean", "--if-exists", "--no-owner",
                                       "-d", "relaydesk"], stdin=dump,
                                      stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=False)
        if restored.returncode:
            raise RuntimeError("isolated pg_restore failed; output withheld")
        restore_seconds = round(time.monotonic() - restore_start, 3)
        if int(sql(target, "SELECT max(version_id) FROM goose_db_version WHERE is_applied;")) != source_migration:
            raise RuntimeError("restored migration version does not match source")
        if sql(target, "SELECT status FROM business_actions WHERE kind='slack_invoice_paid';") != "pending":
            raise RuntimeError("older backup did not contain pending effect")
        # The restored process environment has no outbound clearance. The SQL
        # gate is committed before any worker process could be started.
        if sql(target, "SELECT enter_restore_recovery('Synthetic backup boundary review');") != "1":
            raise RuntimeError("ambiguous effect was not quarantined")
        if sql(target, "SELECT outbound_enabled FROM restore_safety_gate WHERE singleton;") != "f":
            raise RuntimeError("restored database still permits outbound execution")
        if sql(target, "SELECT status||':'||error_class FROM business_actions WHERE kind='slack_invoice_paid';") != "unknown:restore_uncertain":
            raise RuntimeError("ambiguous notification was not unknown")
        denied = subprocess.run(["docker", "exec", "-i", target, "psql", "-X", "-v", "ON_ERROR_STOP=1",
                                 "-U", "relaydesk_migrate", "-d", "relaydesk"],
                                input=b"SELECT clear_restore_recovery('Unreviewed clearance must fail');",
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        if denied.returncode == 0:
            raise RuntimeError("unreviewed notification passed clearance")

        witness = json.loads(external_marker.read_text())
        if witness["channel"] != "C_SYNTHETIC" or witness["ts"] != "1234567890.123456":
            raise RuntimeError("synthetic external evidence mismatch")
        sql(target, """
SELECT decision_id FROM decide_business_action(
 decode(repeat('ab',32),'hex'),
 (SELECT tenant_id FROM business_actions WHERE kind='slack_invoice_paid'),
 (SELECT id FROM business_actions WHERE kind='slack_invoice_paid'),
 'confirm_unknown','Synthetic external witness reviewed after restore',
 (SELECT generation FROM business_actions WHERE kind='slack_invoice_paid'),
 'restore-drill-confirm','C_SYNTHETIC','1234567890.123456',false,'restore-drill');
""")
        sql(target, "SELECT clear_restore_recovery('Synthetic operator review and account binding checked');")
        if sql(target, "SELECT status FROM business_actions WHERE kind='slack_invoice_paid';") != "succeeded":
            raise RuntimeError("operator confirmation did not resolve the effect")
        if sql(target, "SELECT outbound_enabled FROM restore_safety_gate WHERE singleton;") != "t":
            raise RuntimeError("eligible processing did not resume after review")
        # Run the existing CRM current-state executor test against the actual
        # restored database. Both ports are loopback-only and randomly assigned;
        # the broker, credentials and encryption key are synthetic and isolated.
        run(["docker", "run", "-d", "--name", broker, "--network", network,
             "-p", "127.0.0.1::4222", NATS, "--jetstream"])
        started.append(broker)
        db_port = run(["docker", "port", target, "5432/tcp"]).rsplit(":", 1)[-1]
        nats_port = run(["docker", "port", broker, "4222/tcp"]).rsplit(":", 1)[-1]
        for _ in range(60):
            try:
                with socket.create_connection(("127.0.0.1", int(nats_port)), timeout=1):
                    break
            except OSError:
                time.sleep(1)
        else:
            raise RuntimeError("isolated NATS did not become ready")
        env_path = dump_dir / "restored-test.env"
        key = base64.b64encode(secrets.token_bytes(32)).decode()
        database = f"127.0.0.1:{db_port}"
        lines = [f"RELAYDESK_{role + '_' if role else ''}DATABASE_URL=postgres://relaydesk_{user}:{password}@{database}/relaydesk?sslmode=disable"
                 for role, user in (("", "runtime"), ("APP", "app"), ("AUTH", "auth"),
                                    ("MIGRATION", "migrate"))]
        lines.append("RELAYDESK_ENCRYPTION_KEY_B64=" + key)
        with os.fdopen(os.open(env_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "w") as env_file:
            env_file.write("\n".join(lines) + "\n")
        test_env = dict(os.environ)
        test_env.update({"RELAYDESK_TEST_ENV_FILE": str(env_path),
                         "RELAYDESK_TEST_NATS_URL": f"nats://127.0.0.1:{nats_port}",
                         "RELAYDESK_RESTORE_DRILL_TARGET": "1"})
        run(["go", "test", "-tags", "integration", "-count=1", "-run",
             "^TestMission07CRMCurrentStateRepairAfterRecoveryClearance$",
             "./tests/integration"], env=test_env)
        validation_seconds = round(time.monotonic() - restore_start - restore_seconds, 3)
        report = {"result": "PASS", "scope": "isolated synthetic PostgreSQL drill",
                  "source_migration": source_migration, "backup_seconds": backup_seconds,
                  "restore_seconds": restore_seconds, "validation_seconds": validation_seconds,
                  "observed_data_loss_window_seconds": round(failure_at - boundary, 3),
                  "observed_recovery_seconds": round(time.time() - failure_at, 3),
                  "ambiguous_notifications_quarantined": 1,
                  "operator_decision": "confirm_unknown", "outbound_after_clearance": True,
                  "crm_current_state_on_restored_database": "PASS",
                  "real_provider_effects": 0, "dump_bytes": dump_bytes}
        print(json.dumps(report, sort_keys=True))
    finally:
        for container in reversed(started):
            subprocess.run(["docker", "rm", "-f", container], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, check=False)
        subprocess.run(["docker", "network", "rm", network], stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, check=False)
        shutil.rmtree(dump_dir)


if __name__ == "__main__":
    main()
