#!/usr/bin/env python3
"""Stream a PostgreSQL pilot backup directly into age; no plaintext dump file."""

import argparse
import os
from pathlib import Path
import re
import shutil
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]


def age_binary() -> str:
    binary = os.environ.get("RELAYDESK_AGE_BIN") or shutil.which("age")
    if not binary or not Path(binary).is_file():
        raise RuntimeError("age binary unavailable; encrypted backup not created")
    return binary


def private_dir(path: Path) -> Path:
    destination = path.resolve()
    if destination == ROOT or ROOT in destination.parents:
        ignored = subprocess.run(["git", "check-ignore", "-q", str(destination)], cwd=ROOT,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        if ignored.returncode != 0:
            raise RuntimeError("backup directory inside repository is not ignored")
    destination.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(destination, 0o700)
    return destination


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-container", required=True)
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--recipient", required=True, help="public age1 recipient, never the identity key")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{2,100}", args.source_container):
        parser.error("invalid container name")
    if not re.fullmatch(r"age1[023456789acdefghjklmnpqrstuvwxyz]{55,120}", args.recipient):
        parser.error("invalid age public recipient")
    destination = private_dir(args.output_dir)
    path = destination / ("relaydesk-" + time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + ".dump.age")
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    try:
        dump = subprocess.Popen(["docker", "exec", args.source_container, "pg_dump", "-U", "postgres",
                                 "-Fc", "--no-owner", "-d", "relaydesk"],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            with os.fdopen(fd, "wb") as out:
                encrypted = subprocess.run([age_binary(), "-r", args.recipient],
                                           stdin=dump.stdout, stdout=out,
                                           stderr=subprocess.DEVNULL, check=False)
                out.flush()
                os.fsync(out.fileno())
        finally:
            if dump.stdout:
                dump.stdout.close()
        dump_code = dump.wait()
        if dump_code or encrypted.returncode or path.stat().st_size < 100:
            raise RuntimeError("encrypted backup failed; diagnostics withheld")
        print("encrypted pilot backup created: " + str(path))
    except BaseException:
        path.unlink(missing_ok=True)
        raise


if __name__ == "__main__":
    main()
