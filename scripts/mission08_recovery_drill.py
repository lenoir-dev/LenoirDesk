#!/usr/bin/env python3
"""Isolated 20-tenant encrypted-backup and bypassed-restore witness drill."""

import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import time

from mission07_restore_drill import ROOT, POSTGRES, MIGRATE, ready, run, sql


def container(name: str, network: str, password: str) -> None:
    run(["docker", "run", "-d", "--name", name, "--network", network,
         "-p", "127.0.0.1::5432", "-e", "POSTGRES_USER=postgres",
         "-e", "POSTGRES_PASSWORD=" + password, "-e", "POSTGRES_DB=postgres",
         "-e", "RELAYDESK_DB_NAME=relaydesk", "-e", "RELAYDESK_MIGRATE_PASSWORD=" + password,
         "-e", "RELAYDESK_RUNTIME_PASSWORD=" + password,
         "-e", "RELAYDESK_APP_PASSWORD=" + password,
         "-e", "RELAYDESK_AUTH_PASSWORD=" + password,
         "-v", str(ROOT / "deploy/postgres/01-roles.sh") + ":/docker-entrypoint-initdb.d/01-roles.sh:ro",
         POSTGRES])
    ready(name)


def age_tool(name: str) -> str:
    custom = os.environ.get("RELAYDESK_AGE_BIN")
    path = str(Path(custom).with_name(name)) if custom else shutil.which(name)
    if not path or not Path(path).is_file():
        raise RuntimeError("age tooling unavailable")
    return path


def main() -> None:
    suffix = secrets.token_hex(5)
    network = "relaydesk-m08-" + suffix
    source, manual, guarded = ("rd-m08-src-" + suffix, "rd-restore-manual-" + suffix,
                               "rd-restore-guarded-" + suffix)
    private = ROOT / "tmp" / ("mission08-recovery-" + suffix)
    keydir, backups, witnessdir = private / "keys", private / "backups", private / "witness"
    for folder in (private, keydir, backups, witnessdir):
        folder.mkdir(mode=0o700)
        os.chmod(folder, 0o700)
    password = secrets.token_hex(24)  # synthetic isolated containers only
    containers = []
    run(["docker", "network", "create", network])
    try:
        for name in (source, manual, guarded):
            container(name, network, password)
            containers.append(name)
        migration_env = dict(os.environ)
        migration_env["GOOSE_DBSTRING"] = "postgres://relaydesk_migrate:" + password + "@" + source + ":5432/relaydesk?sslmode=disable"
        run(["docker", "run", "--rm", "--network", network, "-e", "GOOSE_DBSTRING",
             "-e", "GOOSE_DRIVER=postgres", "-e", "GOOSE_MIGRATION_DIR=/app/migrations",
             "--entrypoint", "/app/goose", MIGRATE, "up"], env=migration_env)
        sql(source, """
INSERT INTO tenants(name) SELECT 'synthetic-m08-'||g FROM generate_series(1,20) g;
INSERT INTO connections(tenant_id,provider,provider_account_id,mode,status,webhook_ref_hash,
 health_status,execution_environment)
SELECT t.id,'stripe','acct_'||t.name,'test','active',decode(md5(t.name)||md5(t.name||'ref'),'hex'),
 'verified','simulation'
FROM tenants t WHERE t.name LIKE 'synthetic-m08-%';
INSERT INTO automation_revisions(tenant_id,connection_id,revision,status,template_key,config)
SELECT c.tenant_id,c.id,1,'active','subscription_billing_sync.v1',
 '{"slack_channel_id":"C_SYNTHETIC"}'::jsonb FROM connections c;
INSERT INTO webhook_events(tenant_id,connection_id,provider,provider_account_id,mode,event_id,
 event_type,event_api_version,raw_payload,classification,reason,correlation_id,source)
SELECT c.tenant_id,c.id,'stripe',c.provider_account_id,'test',
 'evt_'||c.provider_account_id||'_'||g,'invoice.paid','2026-08-26.dahlia',
 convert_to(repeat('x',1024),'UTF8'),'ignored','synthetic_recovery_scale',
 'synthetic-m08','historical'
FROM connections c CROSS JOIN generate_series(1,300) g;
INSERT INTO business_actions(tenant_id,automation_revision_id,kind,effect_key,
 stripe_connection_id,customer_id,invoice_id)
SELECT r.tenant_id,r.id,'slack_invoice_paid','m08|'||c.provider_account_id,
 c.id,'cus_synthetic_'||c.provider_account_id,'in_synthetic_'||c.provider_account_id
FROM automation_revisions r JOIN connections c ON c.tenant_id=r.tenant_id AND c.id=r.connection_id;
""")
        if sql(source, "SELECT count(*) FROM webhook_events;") != "6000":
            raise RuntimeError("synthetic pilot dataset incomplete")
        identity, wrong = keydir / "identity.txt", keydir / "wrong.txt"
        for path in (identity, wrong):
            result = subprocess.run([age_tool("age-keygen"), "-o", str(path)],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            if result.returncode:
                raise RuntimeError("synthetic key generation failed")
            os.chmod(path, 0o600)
        recipient = run([age_tool("age-keygen"), "-y", str(identity)])
        env = dict(os.environ)
        env["RELAYDESK_AGE_BIN"] = age_tool("age")
        backup_start = time.monotonic()
        run(["python3", str(ROOT / "scripts/pilot_backup_encrypted.py"), "--source-container", source,
             "--output-dir", str(backups), "--recipient", recipient], env=env)
        backup_seconds = round(time.monotonic() - backup_start, 3)
        backup = next(backups.glob("*.dump.age"))
        backup_bytes = backup.stat().st_size

        # A synthetic post-backup effect advances the external authority.
        sql(source, "UPDATE restore_witness_state SET effect_sequence=1 WHERE singleton=true;")
        sql(source, "UPDATE business_actions SET status='succeeded' WHERE id=(SELECT id FROM business_actions ORDER BY id LIMIT 1);")
        incarnation = sql(source, "SELECT incarnation FROM restore_witness_state WHERE singleton=true;")
        state = witnessdir / "state.json"
        with os.fdopen(os.open(state, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), "w") as out:
            json.dump({"version": 1, "incarnation": incarnation, "effect_sequence": 1}, out)

        # An unavailable or wrong key must leave the isolated target untouched.
        wrong_result = subprocess.run(["python3", str(ROOT / "scripts/pilot_restore_encrypted.py"),
                                       "--target-container", guarded, "--backup", str(backup),
                                       "--identity", str(wrong)], env=env, stdout=subprocess.DEVNULL,
                                      stderr=subprocess.DEVNULL, check=False)
        if wrong_result.returncode == 0 or sql(guarded, "SELECT count(*) FROM pg_tables WHERE schemaname='public';") != "0":
            raise RuntimeError("wrong restore key did not fail closed")

        # Intentionally bypass the official helper. The old SQL gate is true,
        # but the external marker must reject this actual pg_restore target.
        manual_start = time.monotonic()
        with backup.open("rb") as ciphertext:
            decrypt = subprocess.Popen([age_tool("age"), "-d", "-i", str(identity)],
                                       stdin=ciphertext, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            restored = subprocess.run(["docker", "exec", "-i", manual, "pg_restore", "-U",
                                       "relaydesk_migrate", "--clean", "--if-exists", "--no-owner",
                                       "-d", "relaydesk"], stdin=decrypt.stdout, stdout=subprocess.DEVNULL,
                                      stderr=subprocess.DEVNULL, check=False)
            if decrypt.stdout:
                decrypt.stdout.close()
            if decrypt.wait() or restored.returncode:
                raise RuntimeError("manual synthetic restore failed")
        manual_seconds = round(time.monotonic() - manual_start, 3)
        if sql(manual, "SELECT outbound_enabled FROM restore_safety_gate WHERE singleton=true;") != "t":
            raise RuntimeError("manual restore did not reproduce the old open SQL gate")
        port = run(["docker", "port", manual, "5432/tcp"]).rsplit(":", 1)[-1]
        check_env = dict(env)
        check_env["RELAYDESK_DATABASE_URL"] = ("postgres://relaydesk_runtime:" + password +
                                                "@127.0.0.1:" + port + "/relaydesk?sslmode=disable")
        check_env["RELAYDESK_RESTORE_WITNESS_PATH"] = str(state)
        blocked = subprocess.run(["go", "run", "./cmd/witness", "check"], cwd=ROOT, env=check_env,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        if blocked.returncode == 0:
            raise RuntimeError("manual restore bypassed external witness")

        restore_start = time.monotonic()
        run(["python3", str(ROOT / "scripts/pilot_restore_encrypted.py"),
             "--target-container", guarded, "--backup", str(backup), "--identity", str(identity)], env=env)
        guarded_seconds = round(time.monotonic() - restore_start, 3)
        if sql(guarded, "SELECT count(*) FROM webhook_events;") != "6000" or \
           sql(guarded, "SELECT count(*) FROM tenants;") != "20" or \
           sql(guarded, "SELECT count(*) FROM business_actions WHERE status='unknown' AND error_class='restore_uncertain';") != "20":
            raise RuntimeError("guarded restore dataset or Slack quarantine incorrect")
        if sql(guarded, "SELECT outbound_enabled FROM restore_safety_gate WHERE singleton=true;") != "f":
            raise RuntimeError("guarded restore opened outbound gate")
        print(json.dumps({"tenants": 20, "event_rows": 6000, "quarantined_slack": 20,
                          "backup_bytes": backup_bytes, "backup_seconds": backup_seconds,
                          "manual_restore_seconds": manual_seconds, "guarded_restore_seconds": guarded_seconds,
                          "wrong_key_rejected": True, "manual_restore_witness_rejected": True,
                          "outbound": "closed_pending_human_review"}, sort_keys=True))
    finally:
        for name in reversed(containers):
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, check=False)
        subprocess.run(["docker", "network", "rm", network], stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, check=False)
        shutil.rmtree(private)


if __name__ == "__main__":
    main()
