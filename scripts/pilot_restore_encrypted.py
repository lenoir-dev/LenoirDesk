#!/usr/bin/env python3
"""Verify age authentication, stream into an isolated restore and quarantine."""

import argparse
import os
from pathlib import Path
import re
import shutil
import subprocess

from pilot_restore import query


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--target-container", required=True)
    parser.add_argument("--backup", required=True, type=Path)
    parser.add_argument("--identity", required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"rd-restore-[A-Za-z0-9-]{4,80}", args.target_container):
        parser.error("target must be an isolated rd-restore-* container")
    backup, identity = args.backup.resolve(strict=True), args.identity.resolve(strict=True)
    if backup.suffix != ".age" or not backup.is_file() or not identity.is_file():
        parser.error("encrypted backup and separate identity file required")
    if backup.stat().st_mode & 0o077 or identity.stat().st_mode & 0o077:
        parser.error("backup and identity must be mode 0600 or stricter")
    age = os.environ.get("RELAYDESK_AGE_BIN") or shutil.which("age")
    if not age or not Path(age).is_file():
        raise RuntimeError("age unavailable; restore not attempted")
    if query(args.target_container, "SELECT count(*) FROM pg_tables WHERE schemaname='public';") != "0":
        parser.error("target database must be empty")
    with backup.open("rb") as encrypted:
        verified = subprocess.run([age, "-d", "-i", str(identity)], stdin=encrypted,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        if verified.returncode:
            raise RuntimeError("encrypted archive authentication failed; restore not attempted")
        encrypted.seek(0)
        decrypt = subprocess.Popen([age, "-d", "-i", str(identity)], stdin=encrypted,
                                   stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            restored = subprocess.run(["docker", "exec", "-i", args.target_container, "pg_restore", "-U",
                                       "relaydesk_migrate", "--clean", "--if-exists", "--no-owner",
                                       "-d", "relaydesk"], stdin=decrypt.stdout, stdout=subprocess.DEVNULL,
                                      stderr=subprocess.DEVNULL, check=False)
        finally:
            if decrypt.stdout:
                decrypt.stdout.close()
        decrypt_code = decrypt.wait()
    if decrypt_code or restored.returncode:
        raise RuntimeError("isolated restore failed; keep all workers disabled")
    quarantined = query(args.target_container,
                        "SELECT enter_restore_recovery('Encrypted isolated pilot restore review required');")
    if query(args.target_container, "SELECT outbound_enabled FROM restore_safety_gate WHERE singleton;") != "f":
        raise RuntimeError("restore recovery gate did not close")
    print("encrypted restore quarantined; outbound disabled; uncertain Slack actions=" + quarantined)


if __name__ == "__main__":
    main()
