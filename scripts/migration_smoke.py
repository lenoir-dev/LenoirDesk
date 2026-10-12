#!/usr/bin/env python3
"""Verify Goose migrations twice against a fresh, disposable local database."""

from pathlib import Path
import secrets
import shutil
import subprocess
import os


def run(args: list[str], env: dict[str,str] | None = None) -> None:
    result = subprocess.run(args, check=False, env=env)
    if result.returncode != 0:
        raise SystemExit("migration smoke command failed (arguments redacted)")


def main() -> None:
    values = dict(
        line.split("=", 1)
        for line in Path(".env").read_text().splitlines()
        if line and not line.startswith("#")
    )
    original = values["RELAYDESK_MIGRATION_DATABASE_URL"]
    if "/relaydesk?" not in original:
        raise SystemExit("migration smoke requires the generated local .env")
    compose = ["docker-compose"] if shutil.which("docker-compose") else ["docker", "compose"]
    database = "relaydesk_smoke_" + secrets.token_hex(6)
    url = original.replace("/relaydesk?", f"/{database}?", 1)
    create = f"CREATE DATABASE {database} OWNER relaydesk_migrate"
    drop = f"DROP DATABASE {database} WITH (FORCE)"
    run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "postgres", "-c", create])
    try:
        for _ in range(2):
            run(compose + ["run", "--rm", "-e", "GOOSE_DBSTRING", "migrate", "up"],{**os.environ,"GOOSE_DBSTRING":url})
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", database,
                       "-At", "-c", "SELECT count(*) FROM public.webhook_events"])
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", database,
                       "-At", "-c", "SELECT count(*) FROM public.user_sessions"])
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", database,
                       "-At", "-c", "SELECT count(*) FROM public.connection_credentials"])
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", database,
                       "-At", "-c", "DO $$ BEGIN IF has_table_privilege('relaydesk_app','public.user_sessions','SELECT') THEN RAISE EXCEPTION 'browser role may read sessions'; END IF; END $$"])
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", database,
                       "-At", "-c", "DO $$ BEGIN IF has_function_privilege('relaydesk_runtime','session_rotate(bytea,bytea,uuid,timestamptz)','EXECUTE') OR has_function_privilege('relaydesk_runtime','workspace_create(bytea,text,text)','EXECUTE') THEN RAISE EXCEPTION 'worker role may modify identity'; END IF; END $$"])
    finally:
        run(compose + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "postgres", "-c", drop])


if __name__ == "__main__":
    main()
