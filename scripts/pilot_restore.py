#!/usr/bin/env python3
"""Restore only to an isolated target and enter durable recovery quarantine."""

import argparse
from pathlib import Path
import re
import subprocess


def query(container: str, statement: str) -> str:
    result = subprocess.run(["docker", "exec", "-i", container, "psql", "-X", "-At",
                             "-v", "ON_ERROR_STOP=1", "-U", "relaydesk_migrate", "-d", "relaydesk"],
                            input=statement.encode(), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            check=False)
    if result.returncode:
        raise RuntimeError("isolated recovery SQL failed; details withheld")
    return result.stdout.decode().strip()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--target-container", required=True)
    parser.add_argument("--backup", required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"rd-restore-[A-Za-z0-9-]{4,80}", args.target_container):
        parser.error("target must be an isolated rd-restore-* container")
    backup = args.backup.resolve(strict=True)
    if backup.suffix != ".dump" or not backup.is_file():
        parser.error("custom-format .dump required")
    if backup.stat().st_mode & 0o077:
        parser.error("backup file must not be group- or world-accessible")
    if query(args.target_container, "SELECT count(*) FROM pg_tables WHERE schemaname='public';") != "0":
        parser.error("target database must be empty; primary database is never overwritten")
    with backup.open("rb") as source:
        result = subprocess.run(["docker", "exec", "-i", args.target_container, "pg_restore", "-U",
                                 "relaydesk_migrate", "--clean", "--if-exists", "--no-owner",
                                 "-d", "relaydesk"], stdin=source, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, check=False)
    if result.returncode:
        raise RuntimeError("pg_restore failed; keep outbound worker disabled")
    quarantined = query(args.target_container,
                        "SELECT enter_restore_recovery('Isolated pilot restore requires external-effect review');")
    if query(args.target_container, "SELECT outbound_enabled FROM restore_safety_gate WHERE singleton;") != "f":
        raise RuntimeError("outbound restore gate did not close")
    print("restore quarantined; outbound disabled; uncertain Slack actions=" + quarantined)


if __name__ == "__main__":
    main()
